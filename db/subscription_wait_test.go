package db

import (
	"os"
	"testing"
	"time"
)

func TestSubscriptionWaitPersistsAndRejectsStaleRecovery(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("quota recovery test", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
	if err = d.WaitForSubscription(task.ID, "worker", "fingerprint", time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := d.SubscriptionWaits(task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
	old := rows[0]
	second, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if wait, err := second.HasSubscriptionWait(task.ID); err != nil || !wait {
		t.Fatalf("restart lost wait: %v %v", wait, err)
	}
	if err = second.WaitForSubscription(task.ID, "worker", "fingerprint", time.Now()); err != nil {
		t.Fatal(err)
	}
	if cleared, err := d.ClearSubscriptionWait(old); err != nil || cleared {
		t.Fatalf("stale recovery cleared new wait: %v %v", cleared, err)
	}
	rows, _ = second.SubscriptionWaits(task.ID)
	if cleared, err := second.ClearSubscriptionWait(rows[0]); err != nil || !cleared {
		t.Fatalf("%v %v", cleared, err)
	}
	if wait, err := d.HasSubscriptionWait(task.ID); err != nil || wait {
		t.Fatalf("did not clear: %v %v", wait, err)
	}
}
