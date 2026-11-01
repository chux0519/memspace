import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace

from memspace_client import _Worker, _owned_root
from paired import clone_snapshot, content_digest, validate_pack


@unittest.skipUnless(os.environ.get("MEMSPACE_TEST_WORKER"), "requires compiled production worker")
class PairedSnapshotTests(unittest.TestCase):
    def test_identical_index_and_read_only_queries_only_switch_packaging(self):
        with tempfile.TemporaryDirectory(prefix="omni-controlled-") as tmp:
            root = Path(tmp).resolve()
            key = hashlib.sha256(b"one-source-identity").hexdigest()
            def client(mode):
                return SimpleNamespace(root=_owned_root(root / mode), binary=Path(os.environ["MEMSPACE_TEST_WORKER"]),
                    mode="context_pack", model="BAAI/bge-m3", evidence_mode=mode,
                    evidence_limit=24, embedding="", timeout=30)
            source_client = client("shadow")
            worker = _Worker(source_client, key)
            messages = [{"role": role, "content": text, "chat_time": "2023-04-01T00:00:00Z"} for role, text in [
                ("user", "For supermarket staging, how many database connections?"),
                ("assistant", "Set supermarket staging database connections to 8; production stays 4."),
                ("user", "How long do we retain supermarket backups?"),
                ("assistant", "Keep supermarket backups for 7 days.")]]
            try:
                worker.rpc("add", session_key="same-source-session", batch_key="complete-session", messages=messages)
                worker.rpc("ready")
            finally:
                worker.stop()
            source = source_client.root / key
            before = content_digest(source)
            outputs = {}
            for mode in ("off", "on"):
                target_client = client(mode)
                target = target_client.root / key
                clone_snapshot(source, target, mode)
                self.assertEqual((source / "memspace.db").read_bytes(), (target / "memspace.db").read_bytes())
                worker = _Worker(target_client, key)
                try:
                    result = worker.rpc("search", query="supermarket staging database connections", top_k=20)
                    validate_pack(result)
                    outputs[mode] = result["pack"]
                finally:
                    worker.stop()
                self.assertEqual(before, content_digest(target))
            self.assertFalse(outputs["off"].get("evidence_groups"))
            self.assertTrue(outputs["on"]["evidence_groups"])
            self.assertEqual(outputs["off"]["project_id"], outputs["on"]["project_id"])

    def test_refuses_uncheckpointed_source_or_overwriting_arm(self):
        with tempfile.TemporaryDirectory(prefix="omni-controlled-") as tmp:
            source = Path(tmp).resolve() / "source"
            source.mkdir()
            (source / "worker.json").write_text(json.dumps({"evidence": {"mode": "shadow"}}))
            (source / "memspace.db").write_bytes(b"test")
            (source / "memspace.db-wal").write_bytes(b"uncheckpointed")
            with self.assertRaises(ValueError):
                clone_snapshot(source, source.parent / "arm", "off")
            (source / "memspace.db-wal").unlink()
            target = source.parent / "arm"
            target.mkdir()
            with self.assertRaises(FileExistsError):
                clone_snapshot(source, target, "off")


if __name__ == "__main__":
    unittest.main()
