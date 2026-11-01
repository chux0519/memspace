#!/usr/bin/env python3
"""Controlled LME ablation: prepare once, copy the same DB, switch packaging.

Uses the production worker and unchanged official answer/judge pipeline. No
credentials or reference answers are sent to the worker. Raw artifacts are
private, and an incomplete execution never becomes an accuracy score.
"""
from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import sqlite3
import statistics
import subprocess
import sys
import threading
import time
from types import SimpleNamespace

from memspace_client import _Worker, _owned_root

PIN = "7c79d46e31dd85f3da2509b430bba9406656dd28"


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def dump(path, value):
    path = Path(path)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    tmp.replace(path)


def validate_harness(root):
    # A source archive has no .git directory; git rev-parse there can silently
    # inspect an unrelated ancestor repository. Pin executable content instead.
    pin_path = Path(__file__).with_name("paired-upstream.json")
    pin = json.loads(pin_path.read_text())
    overlay = json.loads((root / ".memspace-overlay.json").read_text())
    if pin["revision"] != PIN or overlay.get("upstream") != PIN:
        raise ValueError("unsupported official harness revision")
    for name, expected in {**pin["files"], **overlay["files"]}.items():
        if sha(root / name) != expected:
            raise ValueError("official harness file changed: " + name)
    return sha(pin_path)


def content_digest(directory):
    """Hash actual source/index content, excluding runtime provider telemetry."""
    db = sqlite3.connect(f"file:{directory / 'memspace.db'}?mode=ro", uri=True)
    try:
        h = hashlib.sha256()
        for table in ("events", "sessions", "session_summaries", "search_docs",
                      "memories", "evidence_objects", "evidence_links"):
            columns = [r[1] for r in db.execute(f'PRAGMA table_info("{table}")')]
            if not columns:
                continue
            rows = list(db.execute(f'SELECT * FROM "{table}"'))
            # Runtime timestamps stored in source tables are copied too; none
            # should change when a read-only query runs on an indexed snapshot.
            encoded = sorted(json.dumps(row, ensure_ascii=False, default=lambda b: b.hex()) for row in rows)
            h.update(json.dumps([table, columns, encoded]).encode())
        return h.hexdigest()
    finally:
        db.close()


def clone_snapshot(source, target, mode):
    """Only operate on an explicit owned benchmark source, with worker stopped."""
    if mode not in ("off", "on"):
        raise ValueError("snapshot arm must be off or on")
    if source.is_symlink() or source.resolve() != source or not (source / "worker.json").is_file():
        raise ValueError("unowned or aliased source snapshot")
    wal = source / "memspace.db-wal"
    if wal.exists() and wal.stat().st_size:
        raise ValueError("source worker must be closed and WAL checkpointed")
    shutil.copytree(source, target)
    marker = target / "worker.json"
    st = json.loads(marker.read_text())
    st["evidence"]["mode"] = mode
    marker.write_text(json.dumps(st))
    marker.chmod(0o600)
    if sha(source / "memspace.db") != sha(target / "memspace.db"):
        raise ValueError("snapshot byte hash mismatch")


def validate_pack(result):
    p = result["pack"]
    assert len(p["items"]) <= 6 and len(p["text"]) <= 6000
    assert result["texts"] == ([p["text"]] if p["text"] else [])
    assert all(x["project_id"] == p["project_id"] for x in p["items"])
    keys = {x["key"] for x in p["items"]}
    for group in p.get("evidence_groups", []):
        assert all("evidence:" + m["object_id"] in keys for m in group["members"])
        assert not group["complete"] or not any(m["partial"] for m in group["members"])


class Experiment:
    def __init__(self, args):
        self.args = args
        self.out = _owned_root(args.out)
        if any(p.name != ".memspace-omnimemeval.json" for p in self.out.iterdir()):
            raise ValueError("new output directory required; previous attempts are retained")
        self.data = json.loads(args.dataset.read_text())
        if not 0 <= args.start < args.end <= len(self.data):
            raise ValueError("invalid half-open dataset index range")
        harness_pin_sha = validate_harness(args.harness)
        revision = PIN
        for mode in ("off", "on"):
            folder = args.harness / "results/lme" / ("memspace-" + args.version + "_" + mode)
            if folder.exists():
                raise ValueError("official result version already exists; choose a fresh version")
        if args.env_file:
            from dotenv import dotenv_values
            configured = dotenv_values(args.env_file)
            for name, expected in (("ANSWER_MODEL", "gpt-4.1-mini-2025-04-14"), ("EVAL_MODEL", "gpt-4o-mini-2024-07-18")):
                if configured.get(name) != expected:
                    raise ValueError(f"controlled official run requires {name}={expected}")
        sys.path.insert(0, str(args.harness / "scripts"))
        from longmemeval.lme_data import sanitize_lme_message_content
        from longmemeval.lme_common import build_search_result
        from utils.time import parse_lme_time, to_iso
        from utils.prompts import CONTEXT_TEMPLATE
        self.clean, self.entry = sanitize_lme_message_content, build_search_result
        self.parse_time, self.iso, self.template = parse_lme_time, to_iso, CONTEXT_TEMPLATE
        self.dataset_sha = sha(args.dataset)
        self.roots = {mode: _owned_root(self.out / ("stores-" + mode)) for mode in ("shadow", "off", "on")}
        self.lock = threading.Lock()
        self.done, self.failures, self.active = set(), {}, {}
        self.records = {mode: {} for mode in ("off", "on")}
        self.manifest = {
            "dataset_sha256": self.dataset_sha, "worker_sha256": sha(args.binary),
            "driver_sha256": sha(__file__), "harness_revision": revision,
            "prompt_file_sha256": sha(args.harness / "scripts/utils/prompts.py"),
            "harness_pin_manifest_sha256": harness_pin_sha,
            "adapter_client_sha256": sha(Path(__file__).with_name("memspace_client.py")),
            "start": args.start, "end_exclusive": args.end, "expected": args.end - args.start,
            "workers": args.workers, "embedding_url": args.embedding_url,
            "embedding_model": args.model, "timezone": os.environ.get("TZ", "system local timezone"),
            "arms": ["off", "on"], "max_real_sources": 6, "max_chars": 6000,
            "shared_source": "one closed shadow-captured DB per question; byte-identical copies before arm startup",
            "query_order": "off/on for even indices, on/off for odd indices",
            "summary_order": "occurred_at,sequence_number,event_id; fixed worker shared by both arms",
            "answer_model": "gpt-4.1-mini-2025-04-14", "judge_model": "gpt-4o-mini-2024-07-18",
            "limits": ["One stochastic answer/judge run per arm, not human truth",
                       "Not production SLA; shared CPU/cache and worker query timings",
                       "Measures relation packaging over captured edges, not capture cost or RSS",
                       "Same budget ceilings, not identical realized context token counts",
                       "Does not reproduce another vendor's private backend or adapter settings"],
        }
        dump(self.out / "manifest.json", self.manifest)
        self.progress("preparing_and_querying")

    def client(self, mode):
        return SimpleNamespace(root=self.roots[mode], binary=self.args.binary,
                               mode="context_pack", model=self.args.model, evidence_mode=mode,
                               evidence_limit=24, embedding=self.args.embedding_url, timeout=900)

    def progress(self, phase):
        dump(self.out / "progress.json", {"phase": phase, "at": time.time(),
             "expected": self.args.end - self.args.start, "paired_search_completed": len(self.done),
             "failures": self.failures, "active": self.active})

    def unit(self, index):
        row = self.data[index]
        identity = f"lme-source:{self.dataset_sha}:{index}"
        key = hashlib.sha256(identity.encode()).hexdigest()
        source = self.roots["shadow"] / key
        unit_dir = self.out / "units" / f"{index:04d}"
        unit_dir.mkdir(parents=True)
        prepared = _Worker(self.client("shadow"), key)
        try:
            assert len(row["haystack_sessions"]) == len(row["haystack_dates"])
            for session_index, (session, date) in enumerate(zip(row["haystack_sessions"], row["haystack_dates"])):
                stamp = self.iso(self.parse_time(date))
                messages = []
                for turn in session:
                    text = self.clean(turn.get("content", ""))
                    if text is not None and str(text).strip():
                        messages.append({"role": turn["role"], "content": text, "chat_time": stamp})
                if messages:
                    prepared.rpc("add", session_key=f"{identity}:session:{session_index}",
                                 batch_key="complete-session", messages=messages)
                    prepared.rpc("ready")
                with self.lock:
                    self.active[index] = {"session_done": session_index + 1, "sessions": len(row["haystack_sessions"])}
                    self.progress("preparing_and_querying")
        finally:
            prepared.stop()
        snapshot_sha, base_digest = sha(source / "memspace.db"), content_digest(source)
        results, hashes = {}, {}
        order = ("off", "on") if index % 2 == 0 else ("on", "off")
        for mode in order:
            folder = self.roots[mode] / key
            clone_snapshot(source, folder, mode)
            hashes[mode] = sha(folder / "memspace.db")
            worker = _Worker(self.client(mode), key)
            try:
                result = worker.rpc("search", query=row["question"], top_k=20)
                validate_pack(result)
                results[mode] = result
            finally:
                worker.stop()
            if content_digest(folder) != base_digest:
                raise ValueError(f"query mutated source content for {index}/{mode}")
            dump(unit_dir / (mode + ".json"), result)
        assert hashes["off"] == hashes["on"] == snapshot_sha
        dump(unit_dir / "control.json", {"index": index, "source_identity": identity,
             "source_snapshot_sha256": snapshot_sha, "arm_snapshot_sha256": hashes,
             "content_digest_before_and_after": base_digest, "query_order": order,
             "status": "complete"})
        with self.lock:
            for mode in ("off", "on"):
                text = results[mode]["pack"]["text"]
                uid = f"lme_exper_user_{self.args.version}_{mode}_{index}"
                self.records[mode].update(self.entry(row, user_id=uid,
                    context=self.template.format(memories=text), duration_ms=results[mode]["duration_ms"],
                    status="success" if text else "success_empty"))
            self.active.pop(index, None)
            self.done.add(index)
            self.progress("preparing_and_querying")
        # Keep the source snapshot for exact replay; remove only the two owned
        # arm copies after recording source immutability and successful outputs.
        for mode in ("off", "on"):
            folder = self.roots[mode] / key
            marker = json.loads((folder / "worker.json").read_text())
            assert folder.parent == self.roots[mode] and marker["user_key"] == key
            shutil.rmtree(folder)
        print(json.dumps({"paired_completed": len(self.done), "index": index}), flush=True)

    def run(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=self.args.workers) as pool:
            futures = {pool.submit(self.unit, i): i for i in range(self.args.start, self.args.end)}
            for future in concurrent.futures.as_completed(futures):
                try:
                    future.result()
                except Exception as exc:
                    with self.lock:
                        self.failures[futures[future]] = str(exc)
                        self.progress("preparing_and_querying")
        if self.failures:
            self.progress("failed_search")
            raise RuntimeError("paired search incomplete; failures retained, no score computed")
        assert self.done == set(range(self.args.start, self.args.end))
        for mode in ("off", "on"):
            folder = self.args.harness / "results/lme" / ("memspace-" + self.args.version + "_" + mode)
            folder.mkdir(exist_ok=False)
            dump(folder / "memspace_lme_search_results.json", self.records[mode])
            dump(self.out / (mode + "-search.json"), self.records[mode])
        self.progress("search_complete")
        if not self.args.env_file:
            return
        self.progress("answering_and_grading")
        validate_harness(self.args.harness)
        if sha(self.args.binary) != self.manifest["worker_sha256"]:
            raise ValueError("frozen worker changed during measurement")

        def qa(mode):
            command = ["bash", "scripts/run_lme_eval.sh", "--lib", "memspace", "--env", str(self.args.env_file),
                       "--version", self.args.version + "_" + mode, "--from-step", "3", "--to-step", "6",
                       "--llm-workers", "4", "--save-model-input", "1", "--notify", "0"]
            with (self.out / (mode + "-qa.log")).open("w") as log:
                result = subprocess.run(command, cwd=self.args.harness, stdout=log, stderr=subprocess.STDOUT)
            if result.returncode:
                raise RuntimeError(f"official QA failed for {mode}: exit {result.returncode}")

        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            list(pool.map(qa, ("off", "on")))
        self.summarize()
        self.progress("complete")

    def summarize(self):
        paired = {}
        for mode in ("off", "on"):
            folder = self.args.harness / "results/lme" / ("memspace-" + self.args.version + "_" + mode)
            for kind in ("responses", "judged"):
                records = json.loads((folder / f"memspace_lme_{kind}.json").read_text())
                assert {int(k.rsplit("_", 1)[1]) for k in records} == self.done
                assert all(x["status"] == "success" for x in records.values())
                dump(self.out / (mode + "-" + kind + ".json"), records)
                if kind == "judged":
                    paired[mode] = {int(k.rsplit("_", 1)[1]): v for k, v in records.items()}
        rows = []
        for i in sorted(self.done):
            a, b = paired["off"][i], paired["on"][i]
            assert a["question"] == b["question"] and a["golden_answer"] == b["golden_answer"]
            ac, bc = a["llm_judgments"]["judgment_1"], b["llm_judgments"]["judgment_1"]
            assert type(ac) == type(bc) == bool
            rows.append({"index": i, "category": a["category"], "off_correct": ac, "on_correct": bc,
                         "off_tokens": a["nlp_metrics"]["context_tokens"], "on_tokens": b["nlp_metrics"]["context_tokens"]})
        wins = sum(not x["off_correct"] and x["on_correct"] for x in rows)
        losses = sum(x["off_correct"] and not x["on_correct"] for x in rows)
        discordant = wins + losses
        p = min(1, 2 * sum(math.comb(discordant, i) for i in range(min(wins, losses) + 1)) / 2 ** discordant) if discordant else 1
        summary = {"n": len(rows), "off_correct": sum(x["off_correct"] for x in rows),
                   "on_correct": sum(x["on_correct"] for x in rows), "wins": wins, "losses": losses,
                   "unchanged": len(rows) - wins - losses, "mcnemar_exact_p": p,
                   "mean_off_context_tokens": statistics.mean(x["off_tokens"] for x in rows),
                   "mean_on_context_tokens": statistics.mean(x["on_tokens"] for x in rows),
                   "snapshot_controls_passed": len(rows), "default_enabled": False}
        categories = {}
        for row in rows:
            c = categories.setdefault(row["category"], {"n": 0, "off_correct": 0, "on_correct": 0})
            c["n"] += 1
            c["off_correct"] += row["off_correct"]
            c["on_correct"] += row["on_correct"]
        summary["categories"] = categories
        for mode in ("off", "on"):
            packs = [json.loads((self.out / "units" / f"{i:04d}" / (mode + ".json")).read_text()) for i in sorted(self.done)]
            times = sorted(x["duration_ms"] for x in packs)
            summary[mode + "_pack"] = {"p50_ms": statistics.median(times), "p95_ms": times[math.ceil(.95 * len(times)) - 1],
                "mean_chars": statistics.mean(len(x["pack"]["text"]) for x in packs),
                "mean_items": statistics.mean(len(x["pack"]["items"]) for x in packs),
                "empty_packs": sum(not x["pack"]["text"] for x in packs),
                "groups": sum(len(x["pack"].get("evidence_groups", [])) for x in packs),
                "complete_groups": sum(g["complete"] for x in packs for g in x["pack"].get("evidence_groups", []))}
        dump(self.out / "paired.json", rows)
        dump(self.out / "summary.json", summary)
        n = len(rows)
        text = ["MemSpace 同快照关系组包对照", "",
                f"完整{n}题，源DB逐题字节相同，查询前后源内容一致；同worker、模型、预算，仅切off/on。",
                f"关闭：{summary['off_correct']}/{n}（{summary['off_correct']/n:.1%}）；开启：{summary['on_correct']}/{n}（{summary['on_correct']/n:.1%}）。",
                f"变好{wins}题，变差{losses}题，不变{n-wins-losses}题。", "",
                f"平均上下文tokens：{summary['mean_off_context_tokens']:.1f} → {summary['mean_on_context_tokens']:.1f}。",
                "只评价配对组包；不是线上延迟、内存或结构化修正收益的测试。仍是单次模型作答/裁判，人工真值未审核。",
                "新baseline已修复同时间戳排序，不能沿用上轮47.0%作为本轮关闭组得分。线上开关不自动变更。", "",
                "逐题结果、输入和上下文见paired.json、off/on-search/responses/judged.json；源快照保留在stores-shadow，控制证据见units/*/control.json。"]
        text += ["", "|题型|题数|关闭|开启|", "|---|---:|---:|---:|"]
        for category, c in sorted(categories.items()):
            text.append(f"|{category}|{c['n']}|{c['off_correct']/c['n']:.1%}|{c['on_correct']/c['n']:.1%}|")
        text += ["", "|指标|关闭|开启|", "|---|---:|---:|",
                 f"|评测组包p50 ms|{summary['off_pack']['p50_ms']:.1f}|{summary['on_pack']['p50_ms']:.1f}|",
                 f"|评测组包p95 ms|{summary['off_pack']['p95_ms']:.1f}|{summary['on_pack']['p95_ms']:.1f}|",
                 f"|空包|{summary['off_pack']['empty_packs']}|{summary['on_pack']['empty_packs']}|"]
        (self.out / "report.zh.md").write_text("\n".join(text) + "\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    for name in ("dataset", "binary", "harness", "out"):
        ap.add_argument("--" + name, required=True, type=lambda x: Path(x).expanduser().absolute())
    ap.add_argument("--embedding-url", default="")
    ap.add_argument("--model", default="BAAI/bge-m3")
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--start", type=int, default=0)
    ap.add_argument("--end", type=int, default=500)
    ap.add_argument("--version", required=True)
    ap.add_argument("--env-file", type=lambda x: Path(x).expanduser().absolute())
    args = ap.parse_args()
    if args.workers < 1 or not args.version.replace("_", "").isalnum():
        ap.error("positive workers and alphanumeric/underscore version required")
    experiment = Experiment(args)
    try:
        experiment.run()
    except BaseException:
        experiment.progress("failed")
        raise


if __name__ == "__main__":
    main()
