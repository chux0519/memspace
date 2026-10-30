import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("rerank_report", Path(__file__).parent / "rerank/report.py")
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)
score_spec = importlib.util.spec_from_file_location("rerank_score", Path(__file__).parent / "rerank/score.py")
score = importlib.util.module_from_spec(score_spec)
score_spec.loader.exec_module(score)


def row(qid, split, passed=False, negative=False, text=""):
    return {"question_id": qid, "split": split, "text": text,
            "score": {"negative": negative, "all_hit": passed, "out_of_scope": 0, "provenance_errors": 0}}


class RerankCalibrationTests(unittest.TestCase):
    def test_same_source_can_use_different_chunks_at_different_depths(self):
        rows = [{"depth": 12, "query": "q", "candidates": [{"hit": {"ref_id": "doc"}, "source_text": "full"}]},
                {"depth": 24, "query": "q", "candidates": [{"hit": {"ref_id": "doc"}, "source_text": "chunk"}]}]
        pairs = {("q", "full"): {"logit": 1}, ("q", "chunk"): {"logit": 2}}
        scores = score.assemble_scores(rows, pairs)
        self.assertEqual(scores["scores"]["12"]["q"]["doc"], 1)
        self.assertEqual(scores["scores"]["24"]["q"]["doc"], 2)
        rows[1]["depth"] = 12
        with self.assertRaises(ValueError):
            score.assemble_scores(rows, pairs)

    def test_test_results_cannot_choose_threshold_and_new_dev_false_return_rejected(self):
        baseline = [row("q001", "dev", True, text="old"), row("q002", "dev"),
                    row("q073", "dev", negative=True), row("q003", "test")]
        useful = [row("q001", "dev", True, text="old"), row("q002", "dev", True, text="new"),
                  row("q073", "dev", negative=True), row("q003", "test")]
        false_return = [dict(r) for r in useful]
        false_return[2] = row("q073", "dev", negative=True, text="unrelated")
        false_return[3] = row("q003", "test", True, text="huge apparent test benefit")
        groups = {(12, "baseline", 0): baseline, (12, "rescue", 2): useful,
                  (12, "replace", 0): false_return}
        self.assertEqual(report.select_policy(groups), (12, "rescue", 2))
        groups[(12, "baseline", 0)][3] = row("q003", "test", True)
        self.assertEqual(report.select_policy(groups), (12, "rescue", 2))

    def test_missing_attribute_context_is_not_a_clean_negative_error(self):
        s = report.summarize([row("q089", "dev", negative=True, text="related evidence"),
                              row("q097", "dev", negative=True, text="wrong named topic")])
        self.assertEqual(s["clean_negative_nonempty_ids"], ["q097"])
        self.assertEqual(s["unsupported_attribute_nonempty_ids"], ["q089"])


if __name__ == "__main__":
    unittest.main()
