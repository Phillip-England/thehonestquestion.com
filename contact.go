package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

const contactWindow = 24 * time.Hour
const contactLimit = 3
const contactRetention = 365 * 24 * time.Hour
const contactMaxMessages = 1000
const contactPageSize = 50

var errContactLimit = errors.New("contact submission limit reached")

type question struct {
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name,omitempty"`
	Email     string    `json:"email,omitempty"`
	Message   string    `json:"message"`
}

type contactMessage struct {
	ID        int64
	CreatedAt string
	Name      string
	Email     string
	Message   string
}

func pruneContactsTx(tx *sql.Tx, now time.Time) error {
	if _, err := tx.Exec(`DELETE FROM contact_submissions WHERE attempted_at <= ?`, now.Add(-contactWindow).Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM contact_messages WHERE created_at <= ?`, now.Add(-contactRetention).Unix()); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM contact_messages WHERE id NOT IN (
		SELECT id FROM contact_messages ORDER BY created_at DESC, id DESC LIMIT ?
	)`, contactMaxMessages)
	return err
}

func (s *postStore) pruneContacts(now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pruneContactsTx(tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postStore) saveContact(q question, ip string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pruneContactsTx(tx, q.CreatedAt); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM contact_submissions WHERE ip=? AND attempted_at > ?`, ip, q.CreatedAt.Add(-contactWindow).Unix()).Scan(&count); err != nil {
		return err
	}
	if count >= contactLimit {
		// Keep cleanup even when this submission is declined.
		if err := tx.Commit(); err != nil {
			return err
		}
		return errContactLimit
	}
	if _, err := tx.Exec(`INSERT INTO contact_messages (created_at, name, email, message) VALUES (?,?,?,?)`, q.CreatedAt.Unix(), q.Name, q.Email, q.Message); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO contact_submissions (ip, attempted_at) VALUES (?,?)`, ip, q.CreatedAt.Unix()); err != nil {
		return err
	}
	if err := pruneContactsTx(tx, q.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postStore) listContacts(page int) ([]contactMessage, bool, error) {
	if err := s.pruneContacts(time.Now().UTC()); err != nil {
		return nil, false, err
	}
	rows, err := s.db.Query(`SELECT id, created_at, name, email, message FROM contact_messages
		ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, contactPageSize+1, (page-1)*contactPageSize)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	messages := make([]contactMessage, 0, contactPageSize)
	for rows.Next() {
		var message contactMessage
		var createdAt int64
		if err := rows.Scan(&message.ID, &createdAt, &message.Name, &message.Email, &message.Message); err != nil {
			return nil, false, err
		}
		message.CreatedAt = time.Unix(createdAt, 0).UTC().Format("Jan 2, 2006 at 3:04 PM UTC")
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(messages) > contactPageSize {
		return messages[:contactPageSize], true, nil
	}
	return messages, false, nil
}

func (s *postStore) migrateLegacyQuestions(path string) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var done string
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key='contact_jsonl_imported'`).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "1" {
		return os.Remove(path)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var q question
		if err := json.Unmarshal(scanner.Bytes(), &q); err != nil {
			return fmt.Errorf("read legacy contact notes: %w", err)
		}
		if q.CreatedAt.IsZero() {
			return errors.New("legacy contact note has no timestamp")
		}
		if _, err := tx.Exec(`INSERT INTO contact_messages (created_at, name, email, message) VALUES (?,?,?,?)`, q.CreatedAt.Unix(), q.Name, q.Email, q.Message); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta (key,value) VALUES ('contact_jsonl_imported','1')`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}
