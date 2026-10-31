import importlib.util
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch
from memspace_client import MemSpaceClient, _REGISTRY, _shutdown

WORKER=os.environ.get('MEMSPACE_TEST_WORKER')
@unittest.skipUnless(WORKER,'set MEMSPACE_TEST_WORKER to the compiled production bridge')
class ClientIntegration(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)/'stores'
        self.env=patch.dict(os.environ,{'MEMSPACE_WORKER':WORKER,'MEMSPACE_DB_ROOT':str(self.root),'MEMSPACE_EMBEDDING_URL':'','MEMSPACE_MODE':'context_pack'},clear=False);self.env.start();self.client=MemSpaceClient()
    def tearDown(self):_shutdown();self.env.stop();self.tmp.cleanup()
    def messages(self,word):return [{'role':'user','content':word+' database retries','chat_time':'2025-01-01T00:00:00Z'},{'role':'assistant','content':word+' database retries are set to 8.','chat_time':'2025-01-01T00:00:01Z'}]
    def test_two_users_retry_and_cleanup(self):
        self.client.add(self.messages('ALPHA'),'a',session_key='session')
        self.client.add(self.messages('BETA'),'b',session_key='session')
        self.client.add(self.messages('ALPHA'),'a',session_key='session')
        a='\n'.join(self.client.search('ALPHA database retries','a',20));self.assertIn('ALPHA',a);self.assertNotIn('BETA',a)
        b='\n'.join(self.client.search('BETA database retries','b',1));self.assertIn('BETA',b)
        self.client.delete('a');self.client.delete('a')
        self.assertIn('BETA','\n'.join(self.client.search('BETA database retries','b',20)))
        worker=next(w for w in _REGISTRY.values() if w.directory.exists());self.assertEqual(worker.rpc('stats')['events'],2)
    def test_restart_and_conflicting_retry(self):
        self.client.add(self.messages('ALPHA'),'a',session_key='session');self.client.close()
        self.client=MemSpaceClient();self.client.add(self.messages('ALPHA'),'a',session_key='session')
        self.assertIn('ALPHA','\n'.join(self.client.search('ALPHA database retries','a',20)))
        with self.assertRaisesRegex(RuntimeError,'different contents'):self.client.add(self.messages('CHANGED'),'a',session_key='session')
    def test_reject_missing_identity_time_and_unowned_root(self):
        with self.assertRaises(ValueError):self.client.add(self.messages('A'),'a')
        m=self.messages('A');del m[1]['chat_time']
        with self.assertRaises(ValueError):self.client.add(m,'a',session_key='s')
        other=Path(self.tmp.name)/'foreign';other.mkdir();(other/'keep').write_text('keep')
        with patch.dict(os.environ,{'MEMSPACE_DB_ROOT':str(other)}):
            with self.assertRaises(ValueError):MemSpaceClient()
        self.assertEqual((other/'keep').read_text(),'keep')
    def test_topk_and_duplicate_words_are_distinct_events(self):
        for i in range(3):self.client.add(self.messages('ALPHA'),'a',session_key='session'+str(i))
        self.client.search('ALPHA database retries','a',1)
        worker=next(iter(_REGISTRY.values()));self.assertEqual(worker.rpc('stats')['events'],6)
        rows=[json.loads(x) for x in (self.root/(worker.key+'.receipts.jsonl')).read_text().splitlines()]
        packs=[r['response']['data']['pack'] for r in rows if r['request']['op']=='search']
        self.assertLessEqual(len(packs[-1]['items']),1)

if __name__=='__main__':unittest.main()
