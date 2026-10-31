import unittest
from score import allocate, bound

class InputTests(unittest.TestCase):
    def test_anchor_keeps_half_and_fills_unused(self):
        self.assertEqual(allocate([500,500,500],1,100),[25,50,25])
        self.assertEqual(allocate([1,500,1],1,100),[1,98,1])
        self.assertEqual(allocate([1,2,3],1,100),[1,2,3])
    def test_bounds_keep_ends_and_handle_zero(self):
        self.assertEqual(bound(list(range(10)),5),[0,1,2,8,9])
        self.assertEqual(bound([1,2],0),[])

if __name__=='__main__':unittest.main()
