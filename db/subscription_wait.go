package db

import "time"

// Waiting is separate from task lifecycle/user pause. No prompts or credentials
// are stored here. Revision CAS prevents a late poll from clearing a newer limit.
type SubscriptionWait struct {
	TaskID      int64     `json:"task_id"`
	AgentKey    string    `json:"agent_key"`
	Fingerprint string    `json:"-"`
	ProfileID   int64     `json:"-"`
	Revision    int64     `json:"-"`
	NextCheck   time.Time `json:"next_check"`
}

func (d *DB) WaitForSubscription(taskID int64, role, fingerprint string, next time.Time, sourceProfile ...int64) error {
	var profileID int64
	if len(sourceProfile) > 0 {
		profileID = sourceProfile[0]
	}
	_, err := d.Exec(`INSERT INTO task_subscription_waits(task_id,agent_key,fingerprint,next_check,profile_id)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(task_id,agent_key) DO UPDATE SET
 fingerprint=EXCLUDED.fingerprint,next_check=EXCLUDED.next_check,profile_id=EXCLUDED.profile_id,revision=EXCLUDED.revision`, taskID, role, fingerprint, next, profileID)
	return err
}

func (d *DB) HasSubscriptionWait(taskID int64) (bool, error) {
	var wait bool
	err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_subscription_waits WHERE task_id=$1)`, taskID).Scan(&wait)
	return wait, err
}

// taskID=0 lists all waits, including ones restored after process restart.
func (d *DB) SubscriptionWaits(taskID int64) ([]SubscriptionWait, error) {
	rows, err := d.Query(`SELECT task_id,agent_key,fingerprint,revision,next_check,profile_id FROM task_subscription_waits
 WHERE ($1::bigint=0 OR task_id=$1) ORDER BY next_check`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubscriptionWait{}
	for rows.Next() {
		var w SubscriptionWait
		if err = rows.Scan(&w.TaskID, &w.AgentKey, &w.Fingerprint, &w.Revision, &w.NextCheck, &w.ProfileID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (d *DB) ClearSubscriptionWait(w SubscriptionWait) (bool, error) {
	result, err := d.Exec(`DELETE FROM task_subscription_waits WHERE task_id=$1 AND agent_key=$2 AND revision=$3`, w.TaskID, w.AgentKey, w.Revision)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DB) DeferSubscriptionCheck(w SubscriptionWait, next time.Time) error {
	_, err := d.Exec(`UPDATE task_subscription_waits SET next_check=$4 WHERE task_id=$1 AND agent_key=$2 AND revision=$3`, w.TaskID, w.AgentKey, w.Revision, next)
	return err
}
