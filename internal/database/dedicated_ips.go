package database

import (
	"database/sql"
	"errors"
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

const dedicatedIPCols = `id, public_ip, listen_ip, network, label, instance_name, target_ip, snat, created_at, assigned_at`

// CreateDedicatedIP adds an external IP to the admin pool.
func (d *DB) CreateDedicatedIP(publicIP, listenIP, network, label string) (*models.DedicatedIP, error) {
	res, err := d.sql.Exec(
		`INSERT INTO dedicated_ips (public_ip, listen_ip, network, label) VALUES (?, ?, ?, ?)`,
		publicIP, listenIP, network, label,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.GetDedicatedIP(id)
}

// GetDedicatedIP loads one pool IP by id.
func (d *DB) GetDedicatedIP(id int64) (*models.DedicatedIP, error) {
	row := d.sql.QueryRow(`SELECT `+dedicatedIPCols+` FROM dedicated_ips WHERE id = ?`, id)
	ip, err := scanDedicatedIP(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ip, err
}

// ListDedicatedIPs returns every IP in the admin pool.
func (d *DB) ListDedicatedIPs() ([]models.DedicatedIP, error) {
	return d.queryDedicatedIPs(`SELECT ` + dedicatedIPCols + ` FROM dedicated_ips ORDER BY id`)
}

// ListDedicatedIPsForInstance returns every dedicated IP currently assigned to name.
func (d *DB) ListDedicatedIPsForInstance(name string) ([]models.DedicatedIP, error) {
	return d.queryDedicatedIPs(
		`SELECT `+dedicatedIPCols+` FROM dedicated_ips WHERE instance_name = ? ORDER BY id`, name,
	)
}

// ListDedicatedIPsByInstance groups assigned dedicated IPs by instance name.
func (d *DB) ListDedicatedIPsByInstance() (map[string][]models.DedicatedIP, error) {
	all, err := d.queryDedicatedIPs(
		`SELECT ` + dedicatedIPCols + ` FROM dedicated_ips WHERE instance_name != '' ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	out := map[string][]models.DedicatedIP{}
	for _, ip := range all {
		out[ip.InstanceName] = append(out[ip.InstanceName], ip)
	}
	return out, nil
}

// AssignDedicatedIP marks a pool IP as assigned to instanceName.
func (d *DB) AssignDedicatedIP(id int64, instanceName, targetIP string, snat bool) error {
	snatInt := 0
	if snat {
		snatInt = 1
	}
	res, err := d.sql.Exec(
		`UPDATE dedicated_ips
		 SET instance_name = ?, target_ip = ?, snat = ?, assigned_at = ?
		 WHERE id = ?`,
		instanceName, targetIP, snatInt, time.Now().UTC(), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UnassignDedicatedIP returns a pool IP to the available pool.
func (d *DB) UnassignDedicatedIP(id int64) error {
	res, err := d.sql.Exec(
		`UPDATE dedicated_ips
		 SET instance_name = '', target_ip = '', snat = 1, assigned_at = NULL
		 WHERE id = ?`,
		id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDedicatedIP removes an IP from the pool.
func (d *DB) DeleteDedicatedIP(id int64) error {
	res, err := d.sql.Exec(`DELETE FROM dedicated_ips WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameDedicatedIPs updates assigned rows when an instance is renamed.
func (d *DB) RenameDedicatedIPs(oldName, newName string) error {
	_, err := d.sql.Exec(
		`UPDATE dedicated_ips SET instance_name = ? WHERE instance_name = ?`,
		newName, oldName,
	)
	return err
}

func (d *DB) queryDedicatedIPs(query string, args ...any) ([]models.DedicatedIP, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.DedicatedIP
	for rows.Next() {
		ip, err := scanDedicatedIP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ip)
	}
	return out, rows.Err()
}

func scanDedicatedIP(row rowScanner) (*models.DedicatedIP, error) {
	var ip models.DedicatedIP
	var snatInt int
	var created, assigned any
	if err := row.Scan(
		&ip.ID, &ip.PublicIP, &ip.ListenIP, &ip.Network, &ip.Label,
		&ip.InstanceName, &ip.TargetIP, &snatInt, &created, &assigned,
	); err != nil {
		return nil, err
	}
	ip.SNAT = snatInt != 0
	ip.CreatedAt = parseTime(created)
	if assigned != nil {
		t := parseTime(assigned)
		if !t.IsZero() {
			ip.AssignedAt = &t
		}
	}
	return &ip, nil
}
