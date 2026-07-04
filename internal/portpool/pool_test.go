package portpool

import (
	"errors"
	"testing"
)

func TestAllocFreeCycle(t *testing.T) {
	p := New(6000, 6002)

	got := map[int]bool{}
	for range 3 {
		port, err := p.Alloc()
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if port < 6000 || port > 6002 {
			t.Fatalf("Alloc returned %d, outside 6000-6002", port)
		}
		if got[port] {
			t.Fatalf("Alloc returned duplicate port %d", port)
		}
		got[port] = true
	}

	if _, err := p.Alloc(); !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted, got %v", err)
	}

	p.Free(6001)
	port, err := p.Alloc()
	if err != nil {
		t.Fatalf("Alloc after Free: %v", err)
	}
	if port != 6001 {
		t.Fatalf("expected freed port 6001 to be reused, got %d", port)
	}
}

func TestReserve(t *testing.T) {
	p := New(6000, 6009)
	if err := p.Reserve(6005); err != nil {
		t.Fatalf("Reserve(6005): %v", err)
	}
	if err := p.Reserve(6005); err == nil {
		t.Fatal("expected error reserving an already-reserved port")
	}
	if err := p.Reserve(7000); err == nil {
		t.Fatal("expected error reserving a port outside the range")
	}

	// Alloc must skip the reserved port.
	for range 9 {
		port, err := p.Alloc()
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if port == 6005 {
			t.Fatal("Alloc handed out a reserved port")
		}
	}
	if _, err := p.Alloc(); !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted, got %v", err)
	}
}

func TestFreeUnallocatedIsNoop(t *testing.T) {
	p := New(6000, 6001)
	p.Free(6000)
	if n := p.InUse(); n != 0 {
		t.Fatalf("InUse = %d, want 0", n)
	}
}
