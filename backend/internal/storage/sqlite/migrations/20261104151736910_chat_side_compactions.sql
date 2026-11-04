-- +goose Up
CREATE TABLE chat_side_compactions (
 id TEXT PRIMARY KEY,
 side_id TEXT NOT NULL,
 native_home TEXT NOT NULL,
 native_id TEXT NOT NULL,
 epoch TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','settled','not_sent','unknown')),
 historical INTEGER NOT NULL DEFAULT 0 CHECK(historical IN (0,1)),
 closed INTEGER NOT NULL DEFAULT 0 CHECK(closed IN (0,1)),
 record_json TEXT NOT NULL CHECK(json_valid(record_json)),
 CHECK(historical=0 OR state='unknown')
);
CREATE INDEX chat_side_compactions_native ON chat_side_compactions(native_home,native_id);
CREATE TABLE chat_side_contents (
 side_id TEXT PRIMARY KEY,
 content_json TEXT NOT NULL CHECK(json_valid(content_json))
);

-- +goose Down
CREATE TEMP TABLE chat_side_compactions_down_check(ok INTEGER CHECK(ok=1));
INSERT INTO chat_side_compactions_down_check SELECT
 NOT EXISTS(SELECT 1 FROM chat_side_compactions) AND NOT EXISTS(SELECT 1 FROM chat_side_contents);
DROP TABLE chat_side_compactions_down_check;
DROP TABLE chat_side_contents;
DROP TABLE chat_side_compactions;
