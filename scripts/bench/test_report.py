import unittest
from tokenize_report import percentile, summarize


class ReportTests(unittest.TestCase):
    def row(self, negative=False, error=False, layer="context"):
        return dict(question_id="q1", layer=layer, score=dict(negative=negative, any_hit=True, all_hit=False, evidence_recall=0.5, mrr=1.0, returned=1, out_of_scope=0, provenance_errors=0), semantic=dict(calls=1, degraded=0), text="context", duration_ms=10, characters=7, tokens=None, error="failure" if error else "", timeout=error)

    def test_denominators_and_repeat(self):
        positive = self.row()
        negative = self.row(negative=True, error=True)
        result = summarize([positive, positive, negative])
        self.assertEqual(result["unique_questions"], 1)
        self.assertEqual(result["positive_samples"], 2)
        self.assertEqual(result["evidence_recall"], 0.5)
        self.assertEqual(result["negative_nonempty_rate"], 1)
        self.assertEqual(result["timeout_rate"], 1/3)
        self.assertIsNone(result["tokens"])
        self.assertIsNone(summarize([self.row(layer="hook_repeat")])["any_hit"])

    def test_partial_judgments_do_not_invent_precision(self):
        unknown = self.row()
        judged = self.row()
        judged["score"].update(judged_returned=4, judged_irrelevant=1)
        s = summarize([unknown, judged])
        self.assertEqual(s["judged_precision"], .75)
        self.assertEqual(s["judged_returned"], 4)
        self.assertEqual(s["judged_questions"], 1)
        judged["score"].update(judged_returned=0, judged_irrelevant=0)
        self.assertIsNone(summarize([judged])["judged_precision"])
        self.assertIsNone(summarize([unknown])["judged_precision"])

    def test_percentile_includes_tail(self):
        self.assertAlmostEqual(percentile([1, 2, 100], 0.95), 90.2)
        self.assertIsNone(percentile([], 0.5))


if __name__ == "__main__":
    unittest.main()
