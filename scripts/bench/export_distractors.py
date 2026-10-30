"""Freeze private distractor text from a read-only SQLite snapshot.

Project identities are deliberately remapped to the four synthetic projects;
this measures noise sensitivity, not real-project identity preservation.
"""
import argparse
import json
import os
from pathlib import Path
import random
import sqlite3
import tempfile


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("database", type=Path)
    p.add_argument("output", type=Path)
    p.add_argument("--count", type=int, default=2000)
    p.add_argument("--seed", type=int, default=7)
    p.add_argument("--max-chars", type=int, default=0, help="retain only this many characters per document; 0 keeps full text")
    args = p.parse_args()
    if args.max_chars < 0:
        p.error("--max-chars must be nonnegative")
    if args.count < 1:
        p.error("--count must be positive")
    # backup handles WAL correctly. Never open the live DB with MemSpace's
    # migrating Open function or copy the .db file without its WAL.
    source = sqlite3.connect(args.database.resolve().as_uri() + "?mode=ro", uri=True)
    with tempfile.TemporaryDirectory(prefix="memspace-bench-snapshot-") as tmp:
        snap = sqlite3.connect(str(Path(tmp) / "snapshot.db"))
        try:
            source.backup(snap)
            source.close()
            rows = snap.execute("SELECT doc_id, title, body FROM search_docs WHERE kind IN ('command','reply','failure','prompt') AND length(body)>40 ORDER BY doc_id").fetchall()
        finally:
            source.close()
            snap.close()
    random.Random(args.seed).shuffle(rows)
    if len(rows) < args.count:
        p.error(f"only {len(rows)} eligible documents; requested {args.count}")
    projects = ["webapp", "billing", "infra", "notes"]
    docs = []
    for i, (doc_id, title, body) in enumerate(rows[:args.count]):
        text = (title + "\n" + body).strip()
        if args.max_chars:
            text = text[:args.max_chars]
        docs.append(dict(id=f"noise-{doc_id}", project=projects[i % 4], session=f"noise-session-{doc_id}", text=text, evidence=text[:30]))
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(docs, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print(f"froze {len(docs)} private distractors into {args.output}")


if __name__ == "__main__":
    main()
