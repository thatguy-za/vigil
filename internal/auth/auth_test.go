package auth

import (
	"testing"
	"time"
)

func TestHashVerify(t *testing.T) {
	h := Hash("correct horse")
	if !Verify(h, "correct horse") || Verify(h, "wrong") || Verify("garbage", "x") || Verify("", "") {
		t.Fatal("hash verification wrong")
	}
	if Hash("correct horse") == h {
		t.Fatal("hashes must be salted")
	}
}

func TestToken(t *testing.T) {
	a, ah := NewToken()
	b, _ := NewToken()
	if a == b || HashToken(a) != ah || ah == a {
		t.Fatal("token generation wrong")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, 50*time.Millisecond)
	for i := 0; i < 3; i++ {
		if !l.Allow("k") {
			t.Fatal("blocked too early")
		}
		l.Fail("k")
	}
	if l.Allow("k") {
		t.Fatal("not blocked")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("window did not expire")
	}
	l.Fail("k")
	l.Reset("k")
	if !l.Allow("k") {
		t.Fatal("reset failed")
	}
}
