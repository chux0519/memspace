import hashlib
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest


class ExportTests(unittest.TestCase):
    def test_read_only_deterministic_snapshot_and_length_bound(self):
        script = Path(__file__).with_name("export_distractors.py")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            db = root / "source.db"
            c = sqlite3.connect(db)
            c.execute("CREATE TABLE search_docs(doc_id INTEGER, title TEXT, body TEXT, kind TEXT)")
            c.executemany("INSERT INTO search_docs VALUES (?,?,?,?)", [(i, f"title{i}", "固定私有干扰文本" * 30, "reply") for i in range(6)])
            c.commit()
            c.close()
            before = hashlib.sha256(db.read_bytes()).hexdigest()
            for name in ("a.json", "b.json"):
                subprocess.run([sys.executable, str(script), str(db), str(root/name), "--count", "4", "--seed", "7", "--max-chars", "50"], check=True, capture_output=True)
            self.assertEqual((root/"a.json").read_bytes(), (root/"b.json").read_bytes())
            docs = json.loads((root/"a.json").read_text())
            self.assertEqual(len(docs), 4)
            self.assertTrue(all(len(d["text"]) == 50 for d in docs))
            self.assertEqual(before, hashlib.sha256(db.read_bytes()).hexdigest())
            self.assertEqual((root/"a.json").stat().st_mode & 0o777, 0o600)
            duplicate = subprocess.run([sys.executable, str(script), str(db), str(root/"a.json"), "--count", "4"], capture_output=True)
            self.assertNotEqual(duplicate.returncode, 0)


if __name__ == "__main__":
    unittest.main()
