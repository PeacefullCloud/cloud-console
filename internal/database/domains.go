package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// CreateDomain adds a hostname routed to an instance.
func (d *DB) CreateDomain(instanceName, domain string, port int) (*models.Domain, error) {
	res, err := d.sql.Exec(
		`INSERT INTO domains (instance_name, domain, port) VALUES (?, ?, ?)`,
		instanceName, domain, port,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.GetDomain(id)
}

// GetDomain loads one domain row.
func (d *DB) GetDomain(id int64) (*models.Domain, error) {
	row := d.sql.QueryRow(`SELECT id, instance_name, domain, port, created_at FROM domains WHERE id = ?`, id)
	return scanDomain(row)
}

// ListDomains returns every configured domain.
func (d *DB) ListDomains() ([]models.Domain, error) {
	return d.queryDomains(`SELECT id, instance_name, domain, port, created_at FROM domains ORDER BY domain`)
}

// ListDomainsForInstance returns the domains attached to one instance.
func (d *DB) ListDomainsForInstance(name string) ([]models.Domain, error) {
	return d.queryDomains(
		`SELECT id, instance_name, domain, port, created_at FROM domains WHERE instance_name = ? ORDER BY domain`, name)
}

// ListDomainsByInstance groups all domains by instance name.
func (d *DB) ListDomainsByInstance() (map[string][]models.Domain, error) {
	all, err := d.ListDomains()
	if err != nil {
		return nil, err
	}
	out := map[string][]models.Domain{}
	for _, dom := range all {
		out[dom.InstanceName] = append(out[dom.InstanceName], dom)
	}
	return out, nil
}

// DeleteDomain removes a domain mapping.
func (d *DB) DeleteDomain(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM domains WHERE id = ?`, id)
	return err
}

// DeleteDomainsForInstance removes every domain attached to an instance.
func (d *DB) DeleteDomainsForInstance(name string) error {
	_, err := d.sql.Exec(`DELETE FROM domains WHERE instance_name = ?`, name)
	return err
}

// CountDomains returns the number of configured domains.
func (d *DB) CountDomains() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM domains`).Scan(&n)
	return n, err
}

func (d *DB) queryDomains(query string, args ...any) ([]models.Domain, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Domain
	for rows.Next() {
		dom, err := scanDomainRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *dom)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanDomain(row *sql.Row) (*models.Domain, error) {
	dom, err := scanDomainRows(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return dom, err
}

func scanDomainRows(row rowScanner) (*models.Domain, error) {
	var dom models.Domain
	var created any
	if err := row.Scan(&dom.ID, &dom.InstanceName, &dom.Domain, &dom.Port, &created); err != nil {
		return nil, err
	}
	dom.CreatedAt = parseTime(created)
	return &dom, nil
}
