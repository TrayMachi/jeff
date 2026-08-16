package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/local/jeff/internal/conversation"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }
type Row struct{ SessionID, Project, Directory string }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode = WAL;", `CREATE TABLE IF NOT EXISTS conversations (
		chat_id INTEGER NOT NULL, topic_id INTEGER NOT NULL, root_message_id INTEGER NOT NULL,
		session_id TEXT NOT NULL, project TEXT NOT NULL, directory TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		PRIMARY KEY (chat_id, topic_id, root_message_id)
	);`} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}
func (s *Store) GetRow(ctx context.Context, key conversation.Key) (*Row, error) {
	var row Row
	err := s.db.QueryRowContext(ctx, `SELECT session_id, project, directory FROM conversations WHERE chat_id=? AND topic_id=? AND root_message_id=?`, key.ChatID, key.TopicID, key.RootMessageID).Scan(&row.SessionID, &row.Project, &row.Directory)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}
func (s *Store) Set(ctx context.Context, key conversation.Key, sessionID, project, directory string, timestamp int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations (chat_id, topic_id, root_message_id, session_id, project, directory, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, topic_id, root_message_id) DO UPDATE SET session_id=excluded.session_id, project=excluded.project, directory=excluded.directory, updated_at=excluded.updated_at`, key.ChatID, key.TopicID, key.RootMessageID, sessionID, project, directory, timestamp, timestamp)
	return err
}
func (s *Store) Close() error { return s.db.Close() }
