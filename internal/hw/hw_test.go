package hw

import "testing"

func TestParseCPUInfo(t *testing.T) {
	cases := []struct {
		name, in, model string
		board           int
	}{
		{"pi 3b+", "Hardware\t: BCM2835\nRevision\t: a020d3\nModel\t\t: Raspberry Pi 3 Model B Plus Rev 1.3\n", "Raspberry Pi 3 Model B Plus Rev 1.3", 3},
		{"pi 4", "Hardware\t: BCM2711\nModel\t\t: Raspberry Pi 4 Model B Rev 1.4\n", "Raspberry Pi 4 Model B Rev 1.4", 4},
		{"pi 400", "Model\t\t: Raspberry Pi 400 Rev 1.0\n", "Raspberry Pi 400 Rev 1.0", 4},
		{"pi 5", "Model\t\t: Raspberry Pi 5 Model B Rev 1.0\n", "Raspberry Pi 5 Model B Rev 1.0", 5},
		{"pi 4 compute module", "Model\t\t: Raspberry Pi Compute Module 4 Rev 1.0\n", "Raspberry Pi Compute Module 4 Rev 1.0", 4},
		{"no model line", "Hardware\t: BCM2837\n", "Raspberry Pi 3", 3},
		{"other board", "Hardware\t: Rockchip RK3588\n", "", 0},
	}
	for _, c := range cases {
		m, b := parseCPUInfo(c.in)
		if m != c.model || b != c.board {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", c.name, m, b, c.model, c.board)
		}
	}
}
