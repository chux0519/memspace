import tempfile
import unittest
from pathlib import Path
from judge_compare import parse_label, read_prompts, summarize

class JudgeCompareTests(unittest.TestCase):
    def test_malformed_label_is_error(self):
        self.assertTrue(parse_label('result: {"label":"correct"}'))
        self.assertFalse(parse_label('{"label": "WRONG"}'))
        for text in ['CORRECT', '{"label":"unknown"}', '{"correct":true}', '', '{"label":\'CORRECT\'}']:
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_label(text)

    def test_errors_are_visible_and_denominator_is_explicit(self):
        records = [
            {'user_id':'a','status':'success','reference_correct':True,'candidate_correct':True},
            {'user_id':'b','status':'success','reference_correct':False,'candidate_correct':True},
            {'user_id':'c','status':'success','reference_correct':True,'candidate_correct':False},
            {'user_id':'d','status':'error','reference_correct':True},
        ]
        result = summarize(records, 4)
        self.assertEqual(result['status'], 'incomplete')
        self.assertEqual(result['errors'], 1)
        self.assertEqual(result['paired_valid'], 3)
        self.assertEqual(result['agreement'], 1)
        self.assertEqual(result['reference_correct_all'], 3)
        self.assertEqual(result['reference_correct_paired'], 2)
        self.assertEqual(result['reference_wrong_candidate_correct'], 1)
        self.assertEqual(result['reference_correct_candidate_wrong'], 1)
        self.assertEqual(result['disagreement_user_ids'], ['b','c'])
        self.assertIsNone(summarize([],1)['agreement_rate_on_valid_pairs'])

    def test_prompt_integrity_checked_without_executing_checkout(self):
        with tempfile.TemporaryDirectory() as folder:
            p = Path(folder)/'prompts.py'
            p.write_text('raise RuntimeError("must not run")')
            with self.assertRaisesRegex(ValueError,'differs'):
                read_prompts(p)

if __name__ == '__main__':
    unittest.main()
