package db

import "fmt"

// SetRunTestSimulatorLease uses a compare-and-swap so a stale response cannot
// replace a different operator binding. The caller validates identity first.
func (d *DB) SetRunTestSimulatorLease(runID, raw string, previous *string) error {
	result, err := d.sql.Exec(`UPDATE runs SET test_simulator_lease=?, updated_at=? WHERE id=? AND test_simulator_lease IS ?`, raw, now(), runID, previous)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("simulator lease changed while the response was being recorded")
	}
	return nil
}
