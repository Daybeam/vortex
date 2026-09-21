package core

import "testing"

func TestCacheSentinel_NoAlertOnSmallPrompt(t *testing.T) {
	c := NewCacheSentinel()
	for i := 0; i < 5; i++ {
		alert, _ := c.Observe("t1", "s1", 1000, 0)
		if alert {
			t.Fatal("small prompt with cache==0 should not alert")
		}
	}
}

func TestCacheSentinel_NoAlertOnCacheHit(t *testing.T) {
	c := NewCacheSentinel()
	for i := 0; i < 5; i++ {
		alert, _ := c.Observe("t1", "s1", 10000, 5000)
		if alert {
			t.Fatal("non-zero cache read should not alert")
		}
	}
}

func TestCacheSentinel_AlertsAfter3ConsecutiveZero(t *testing.T) {
	c := NewCacheSentinel()
	for i := 0; i < 2; i++ {
		alert, n := c.Observe("t1", "s1", 10000, 0)
		if alert {
			t.Fatalf("round %d: should not alert yet (threshold=3)", i+1)
		}
		if n != i+1 {
			t.Fatalf("round %d: expected count %d, got %d", i+1, i+1, n)
		}
	}
	alert, n := c.Observe("t1", "s1", 10000, 0)
	if !alert {
		t.Fatal("3rd consecutive zero-cache round should alert")
	}
	if n != 3 {
		t.Fatalf("expected count 3, got %d", n)
	}
}

func TestCacheSentinel_ResetOnCacheHit(t *testing.T) {
	c := NewCacheSentinel()
	c.Observe("t1", "s1", 10000, 0)
	c.Observe("t1", "s1", 10000, 0)
	c.Observe("t1", "s1", 10000, 5000)
	alert, n := c.Observe("t1", "s1", 10000, 0)
	if alert {
		t.Fatal("cache hit should reset counter; 1 round after reset should not alert")
	}
	if n != 1 {
		t.Fatalf("expected count 1 after reset, got %d", n)
	}
}

func TestCacheSentinel_IndependentSteps(t *testing.T) {
	c := NewCacheSentinel()
	c.Observe("t1", "s1", 10000, 0)
	c.Observe("t1", "s1", 10000, 0)
	alert, _ := c.Observe("t1", "s2", 10000, 0)
	if alert {
		t.Fatal("different step should have independent counter")
	}
}

func TestCacheSentinel_ManualReset(t *testing.T) {
	c := NewCacheSentinel()
	c.Observe("t1", "s1", 10000, 0)
	c.Observe("t1", "s1", 10000, 0)
	c.Reset("t1", "s1")
	alert, n := c.Observe("t1", "s1", 10000, 0)
	if alert || n != 1 {
		t.Fatal("manual reset should clear counter")
	}
}

func TestBudgetSentinel_NoAlertBelow60(t *testing.T) {
	b := NewBudgetSentinel()
	for pct := 10; pct < 60; pct += 10 {
		if tier := b.Observe("t1", pct, 0); tier != 0 {
			t.Fatalf("pct=%d should not alert, got tier %d", pct, tier)
		}
	}
}

func TestBudgetSentinel_AlertsAt60_80_90(t *testing.T) {
	b := NewBudgetSentinel()
	if tier := b.Observe("t1", 60, 0); tier != 60 {
		t.Fatalf("60%% should alert tier 60, got %d", tier)
	}
	if tier := b.Observe("t1", 60, 0); tier != 0 {
		t.Fatal("60% should not re-fire")
	}
	if tier := b.Observe("t1", 80, 0); tier != 80 {
		t.Fatalf("80%% should alert tier 80, got %d", tier)
	}
	if tier := b.Observe("t1", 90, 0); tier != 90 {
		t.Fatalf("90%% should alert tier 90, got %d", tier)
	}
	if tier := b.Observe("t1", 95, 0); tier != 0 {
		t.Fatal("no new tier above 90")
	}
}

func TestBudgetSentinel_PredictionAlert(t *testing.T) {
	b := NewBudgetSentinel()
	if tier := b.Observe("t1", 50, 85); tier != 80 {
		t.Fatalf("predicted 85%% should fire tier 80, got %d", tier)
	}
}

func TestBudgetSentinel_Reset(t *testing.T) {
	b := NewBudgetSentinel()
	b.Observe("t1", 60, 0)
	b.Observe("t1", 80, 0)
	b.Reset("t1")
	if tier := b.Observe("t1", 60, 0); tier != 60 {
		t.Fatalf("after reset, 60%% should re-fire, got %d", tier)
	}
}

func TestBudgetSentinel_IndependentTasks(t *testing.T) {
	b := NewBudgetSentinel()
	b.Observe("t1", 60, 0)
	if tier := b.Observe("t2", 60, 0); tier != 60 {
		t.Fatalf("different task should have independent state, got %d", tier)
	}
}
