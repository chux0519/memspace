import copy
import unittest

from trace_pack import STAGES, analyze, summarize


def receipt(qid="question", gold=("a", "b")):
    def hit(s):
        return dict(doc_id=ord(s), session_id=s, match="all", matched_terms=1, total_terms=1)
    def item(s):
        return dict(key="doc:" + str(ord(s)), ref="event-" + s, kind="history")
    trace = [dict(stage=s, hits=[hit("a"), hit("b")]) for s in STAGES[:5]]
    trace += [dict(stage="sections", items=[item("a"), item("b")]),
              dict(stage="rendered", items=[item("a"), item("b")], text="delivered evidence")]
    return dict(question_id=qid, gold_sessions=list(gold), trace=trace,
                trace_session_sources={"a": "a", "b": "b"}, returned_sessions=["a", "b"],
                pack=dict(items=[item("a"), item("b")], text="delivered evidence"))


class TraceAttributionTest(unittest.TestCase):
    def test_first_failure_is_not_the_last_place_with_missing_evidence(self):
        row = receipt()
        # b is cut by the candidate cap; a survives until admission, then is lost.
        row["trace"][1]["hits"] = row["trace"][1]["hits"][:1]
        for event in row["trace"][2:5]:
            event["hits"] = []
        for event in row["trace"][5:]:
            event["items"] = []
        row["returned_sessions"] = []
        row["pack"]["items"] = []
        result = analyze(row)
        self.assertEqual(result["first_failure"], "deduplicated")
        self.assertEqual(result["source_first_failure"], {"a": "admitted", "b": "deduplicated"})
        self.assertEqual(result["losses"]["admitted"], ["a"])
        self.assertFalse(result["losses"]["selected"])

    def test_abstention_does_not_inflate_complete_or_failure_counts(self):
        positive, abstention = receipt(), receipt("question_abs", ())
        rows = {r["question_id"]: r for r in [positive, abstention]}
        report = summarize(rows, [analyze(r) for r in rows.values()], copy.deepcopy(rows))
        self.assertEqual((report["scored"], report["unscored"], report["final_failures"]), (1, 1, 0))
        self.assertEqual(report["stages"]["rendered"]["rate"], 1)
        self.assertEqual(report["baseline_changes"], dict(source_rankings=[], text=[], items=[]))

    def test_unmapped_sources_and_wrong_final_receipt_are_errors(self):
        row = receipt()
        del row["trace_session_sources"]["b"]
        with self.assertRaisesRegex(ValueError, "unmapped"):
            analyze(row)
        row = receipt()
        row["returned_sessions"] = ["a"]
        with self.assertRaisesRegex(ValueError, "scored sources differ"):
            analyze(row)

    def test_missing_stage_is_not_silently_reported_as_a_loss(self):
        row = receipt()
        del row["trace"][2]
        with self.assertRaisesRegex(ValueError, "incomplete trace"):
            analyze(row)


if __name__ == "__main__":
    unittest.main()
