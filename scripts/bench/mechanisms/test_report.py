import unittest
from report import auc, evaluate

class ReportTests(unittest.TestCase):
    def test_auc_ties(self):
        self.assertEqual(auc([1,2],[0,1]),.875)
        self.assertIsNone(auc([], [1]))
    def test_visible_groups_need_refs_and_text(self):
        h={'doc_id':1,'ref_id':'a','project_id':'p','session_id':'s'}
        c={'request':{'project':{'project_id':'p'}},'candidates':[{'window':[{'hit':h}]}]}
        label={'question':{'evidence_groups':[['a']], 'category':'test'},'documents':{'a':{'evidence':'actual value'}}}
        p={'text':'wrong value','items':[{'key':'doc:1','ref':'a','project_id':'p'}]}
        self.assertFalse(evaluate(c,p,label,'fixture')['all'])
        p['text']='actual value';self.assertTrue(evaluate(c,p,label,'fixture')['all'])
        p['items'][0]['project_id']='foreign'
        with self.assertRaises(ValueError):evaluate(c,p,label,'fixture')

if __name__=='__main__':unittest.main()
