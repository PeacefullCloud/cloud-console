package database

import (
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

// InsertMetric stores one monitoring sample.
func (d *DB) InsertMetric(m *models.Metric) error {
	_, err := d.sql.Exec(`
		INSERT INTO metrics (instance_name, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total, net_rx_bytes, net_tx_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.InstanceName, m.TS.UTC().Format(timeLayout), m.CPUPct,
		m.MemUsed, m.MemTotal, m.DiskUsed, m.DiskTotal, m.NetRxBytes, m.NetTxBytes)
	return err
}

// InsertMetrics stores a whole sampling round in one transaction: one commit
// (and one fsync) instead of one per instance.
func (d *DB) InsertMetrics(batch []models.Metric) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT INTO metrics (instance_name, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total, net_rx_bytes, net_tx_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i := range batch {
		m := &batch[i]
		if _, err := stmt.Exec(m.InstanceName, m.TS.UTC().Format(timeLayout), m.CPUPct,
			m.MemUsed, m.MemTotal, m.DiskUsed, m.DiskTotal, m.NetRxBytes, m.NetTxBytes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListMetrics returns samples for an instance since a point in time, oldest first.
func (d *DB) ListMetrics(instanceName string, since time.Time) ([]models.Metric, error) {
	rows, err := d.sql.Query(`
		SELECT id, instance_name, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total, net_rx_bytes, net_tx_bytes
		FROM metrics WHERE instance_name = ? AND ts >= ? ORDER BY ts ASC`,
		instanceName, since.UTC().Format(timeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Metric
	for rows.Next() {
		var m models.Metric
		var ts any
		if err := rows.Scan(&m.ID, &m.InstanceName, &ts, &m.CPUPct, &m.MemUsed, &m.MemTotal,
			&m.DiskUsed, &m.DiskTotal, &m.NetRxBytes, &m.NetTxBytes); err != nil {
			return nil, err
		}
		m.TS = parseTime(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// LatestMetric returns the most recent sample for an instance.
func (d *DB) LatestMetric(instanceName string) (*models.Metric, error) {
	row := d.sql.QueryRow(`
		SELECT id, instance_name, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total, net_rx_bytes, net_tx_bytes
		FROM metrics WHERE instance_name = ? ORDER BY ts DESC LIMIT 1`, instanceName)

	var m models.Metric
	var ts any
	err := row.Scan(&m.ID, &m.InstanceName, &ts, &m.CPUPct, &m.MemUsed, &m.MemTotal,
		&m.DiskUsed, &m.DiskTotal, &m.NetRxBytes, &m.NetTxBytes)
	if err != nil {
		return nil, err
	}
	m.TS = parseTime(ts)
	return &m, nil
}

// LatestMetricsMap returns the newest sample for every instance, keyed by
// instance name. One query keeps the instances list fast.
func (d *DB) LatestMetricsMap() (map[string]models.Metric, error) {
	rows, err := d.sql.Query(`
		SELECT m.instance_name, m.ts, m.cpu_pct, m.mem_used, m.mem_total,
		       m.disk_used, m.disk_total, m.net_rx_bytes, m.net_tx_bytes
		FROM metrics m
		JOIN (
			SELECT instance_name, MAX(ts) AS ts FROM metrics GROUP BY instance_name
		) latest ON latest.instance_name = m.instance_name AND latest.ts = m.ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]models.Metric{}
	for rows.Next() {
		var m models.Metric
		var ts any
		if err := rows.Scan(&m.InstanceName, &ts, &m.CPUPct, &m.MemUsed, &m.MemTotal,
			&m.DiskUsed, &m.DiskTotal, &m.NetRxBytes, &m.NetTxBytes); err != nil {
			return nil, err
		}
		m.TS = parseTime(ts)
		out[m.InstanceName] = m
	}
	return out, rows.Err()
}

// PruneMetrics deletes samples older than a cutoff.
func (d *DB) PruneMetrics(before time.Time) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM metrics WHERE ts < ?`, before.UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteMetricsForInstance removes samples for a deleted instance.
func (d *DB) DeleteMetricsForInstance(instanceName string) error {
	_, err := d.sql.Exec(`DELETE FROM metrics WHERE instance_name = ?`, instanceName)
	return err
}
