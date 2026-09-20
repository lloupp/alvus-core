package credentials

import (
	"testing"
	"time"
)

func TestRoundRobinCooldownAndDisable(t *testing.T) {
	p := New([]string{"a", "b"})
	now := time.Now()
	i1, k1, err := p.Next(now)
	if err != nil {
		t.Fatal(err)
	}
	i2, k2, err := p.Next(now)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatalf("expected rotation, got %q twice", k1)
	}
	p.Cooldown(i1, now.Add(time.Minute))
	_, k, err := p.Next(now)
	if err != nil {
		t.Fatal(err)
	}
	if k != k2 {
		t.Fatalf("cooled key selected: %q", k)
	}
	p.Disable(i2)
	if _, _, err := p.Next(now); err == nil {
		t.Fatal("expected no available keys")
	}
}
