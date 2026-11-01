import unittest
from score import question_scores, score_rows

class SourceMetricsTest(unittest.TestCase):
    def test_one_source_is_not_all_sources(self):
        m = question_scores(['a', 'b'], ['noise', 'a'])
        self.assertEqual(m['any_hit_at_5'], 1)
        self.assertEqual(m['fractional_at_5'], .5)
        self.assertEqual(m['recall_all_at_5'], 0)

    def test_distinct_sources_not_duplicate_slots(self):
        m = question_scores(['a', 'b'], ['x', 'x', 'x', 'x', 'a', 'b'])
        self.assertEqual(m['recall_all_at_5'], 1)

    def test_abs_excluded_even_with_nonempty_source_labels(self):
        base = {'haystack_session_ids': ['a'], 'answer_session_ids': ['a'],
                'question_type': 'example'}
        data = [dict(base, question_id='p'), dict(base, question_id='n_abs')]
        s, rows, evidence = score_rows(data, {'p': [], 'n_abs': ['a']}, 'fixture')
        self.assertEqual(s['scored'], 1)
        self.assertEqual(s['metrics']['any_hit_at_5'], 0)
        self.assertIsNone(rows[1]['any_hit_at_5'])
        self.assertEqual(evidence[1]['gold_sessions'], [])

    def test_missing_or_foreign_sources_rejected(self):
        data = [{'question_id': 'p', 'haystack_session_ids': ['a'],
                 'answer_session_ids': ['a'], 'question_type': 'example'}]
        for ranks in ({}, {'p': ['foreign']}):
            with self.assertRaises(ValueError):
                score_rows(data, ranks, 'fixture')

if __name__ == '__main__':
    unittest.main()
