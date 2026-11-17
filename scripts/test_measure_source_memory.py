import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import measure_source_memory as sampler


class MemorySamplerTest(unittest.TestCase):
    def test_proc_units_and_disappeared_process(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = Path(tmp)
            (proc / "10").mkdir()
            (proc / "10" / "status").write_text("PPid:\t1\nVmRSS:\t32 kB\nVmHWM:\t64 kB\n")
            (proc / "11").mkdir()  # Process exited before status could be read.
            (proc / "self").mkdir()
            self.assertEqual(sampler.processes(proc), {10: dict(ppid=1, rss=32768, hwm=65536)})

    def test_descendants_and_simultaneous_peak(self):
        first = {10: dict(ppid=1, rss=100, hwm=200), 11: dict(ppid=10, rss=50, hwm=80),
                 12: dict(ppid=11, rss=10, hwm=10), 20: dict(ppid=1, rss=999, hwm=999)}
        second = {10: dict(ppid=1, rss=120, hwm=200)}
        self.assertEqual(sampler.descendants(first, 10), {11, 12})
        with patch.object(sampler, "processes", side_effect=[first, second, {}]), \
                patch.object(sampler.time, "sleep"):
            result = sampler.measure(10, 30, 0.1)
        self.assertEqual(result["samples"], 2)
        self.assertEqual(result["parentPeakSampledRSSBytes"], 120)
        self.assertEqual(result["childrenPeakSampledRSSBytes"], 60)
        self.assertEqual(result["combinedPeakSampledRSSBytes"], 160)  # Not 120 + 60.
        self.assertEqual(result["observedChildProcesses"], 2)


if __name__ == "__main__":
    unittest.main()
