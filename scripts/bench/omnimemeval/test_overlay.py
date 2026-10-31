import json
from pathlib import Path
import shutil
import tempfile
import unittest
from install_adapter import install,HERE

class OverlayTests(unittest.TestCase):
    def test_changed_upstream_rejected_before_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);name=next(iter(json.loads((HERE/'upstream.json').read_text())['files']))
            p=root/name;p.parent.mkdir(parents=True);p.write_text('foreign edit')
            with self.assertRaises(ValueError):install(root)
            self.assertEqual(p.read_text(),'foreign edit')
            self.assertFalse((root/'scripts/client_factory/memspace_client.py').exists())
if __name__=='__main__':unittest.main()
