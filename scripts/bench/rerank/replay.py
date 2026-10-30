"""Replay frozen model scores through real contextpack code in a scratch worktree.

Only the disposable worktree receives the experimental Go adapter. Normal product
sources remain unchanged. No model score is derived from evidence labels.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--scores", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--corpus", type=Path, default=Path("scripts/bench/data/coding-v2-evidence.json"))
    ap.add_argument("--query-vectors", required=True, type=Path)
    ap.add_argument("--embedding-url", default="http://127.0.0.1:8771/v1/embeddings")
    ap.add_argument("--candidate-floor", type=float, default=-1)
    ap.add_argument("--deep48", action="store_true")
    ap.add_argument("--base-commit", default="HEAD", help="production revision to evaluate in the disposable worktree")
    args = ap.parse_args()
    root = Path(__file__).resolve().parents[3]
    out, scores = args.out.resolve(), args.scores.resolve()
    if out.exists():
        raise SystemExit("output must not exist")
    with tempfile.TemporaryDirectory(prefix="memspace-rerank-") as scratch:
        work = Path(scratch) / "repo"
        subprocess.run(["git", "worktree", "add", "--detach", str(work), args.base_commit], cwd=root, check=True)
        try:
            shutil.copytree(root / "scripts/bench/retrieval", work / "scripts/bench/retrieval", dirs_exist_ok=True)
            shutil.copy2(args.corpus, work / "scripts/bench/data/coding-v2-evidence.json")
            def replace(path, old, new):
                p = work / path
                s = p.read_text()
                if s.count(old) != 1:
                    raise ValueError(f"unexpected source version: {path}: {old}")
                p.write_text(s.replace(old, new))
            replace("internal/contextpack/contextpack.go", "Limit: limit * 2,", "Limit: benchDepth(limit * 2),")
            replace("internal/contextpack/contextpack.go", "for _, h := range historyCandidates(resp.Results, current, inject) {",
                    "ranked, rankErr := benchRank(query, resp.Results, inject)\nif rankErr != nil { return nil, rankErr }\nfor _, h := range historyCandidates(ranked, current, inject) {")
            replace("internal/contextpack/contextpack.go", "strongHistoryHit(h, inject))", "strongHistoryHit(h, inject) || benchStrong(h))")
            replace("internal/contextpack/history.go", "!useHistoryHit(h, inject)", "!benchQualifies(h, inject)")
            replace("scripts/bench/retrieval/main.go", "if *diagnosticOnly {", "if os.Getenv(\"MEMSPACE_BENCH_RERANK_STUDY\") == \"1\" { return replayStudy(ctx,d,c,docs,*out) }\nif *diagnosticOnly {")
            for src, dest in [("adapter.go.txt", "internal/contextpack/rerank_bench.go"),
                              ("study.go.txt", "scripts/bench/retrieval/rerank_study.go")]:
                shutil.copy2(Path(__file__).parent / src, work / dest)
            env = dict(os.environ, CGO_ENABLED="1", MEMSPACE_BENCH_RERANK_STUDY="1", MEMSPACE_BENCH_RERANK_SCORES=str(scores))
            if args.deep48:
                env["MEMSPACE_BENCH_RERANK_DEEP48"] = "1"
            subprocess.run(["go", "test", "-tags", "sqlite_fts5", "./internal/contextpack", "./scripts/bench/retrieval"], cwd=work, env=env, check=True)
            subprocess.run(["go", "run", "-tags", "sqlite_fts5", "./scripts/bench/retrieval", "-corpus", "scripts/bench/data/coding-v2-evidence.json", "-out", str(out), "-embedding-url", args.embedding_url, "-query-vectors", str(args.query_vectors.resolve()), "-candidate-floor", str(args.candidate_floor), "-repeats", "1"], cwd=work, env=env, check=True)
            patch = subprocess.check_output(["git", "diff"], cwd=work)
            (out / "adapter.patch").write_bytes(patch)
            for src in ["internal/contextpack/rerank_bench.go", "scripts/bench/retrieval/rerank_study.go"]:
                shutil.copy2(work / src, out / (Path(src).name + ".txt"))
            source_hashes = {}
            sources = list((work / "scripts/bench/retrieval").glob("*.go"))
            sources += [work / p for p in ["internal/contextpack/contextpack.go", "internal/contextpack/history.go",
                                          "internal/model/model.go", "internal/search/query.go", "go.mod", "go.sum"]]
            for source in sources:
                relative = source.relative_to(work)
                dest = out / "source" / relative
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source, dest)
                source_hashes[str(relative)] = hashlib.sha256(source.read_bytes()).hexdigest()
            meta = {"base_commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=work, text=True).strip(),
                    "scores_sha256": hashlib.sha256(scores.read_bytes()).hexdigest(),
                    "query_vectors_sha256": hashlib.sha256(args.query_vectors.read_bytes()).hexdigest(),
                    "corpus_sha256": hashlib.sha256(args.corpus.read_bytes()).hexdigest(),
                    "adapter_patch_sha256": hashlib.sha256(patch).hexdigest(),
                    "source_sha256": source_hashes,
                    "experimental_candidate_floor": args.candidate_floor,
                    "scope": "real context Build with offline score replay; excludes model inference, not live latency"}
            (out / "manifest.json").write_text(json.dumps(meta, indent=2) + "\n")
        finally:
            subprocess.run(["git", "worktree", "remove", "--force", str(work)], cwd=root, check=True)


if __name__ == "__main__":
    main()
