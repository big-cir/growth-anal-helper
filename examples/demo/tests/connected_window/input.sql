-- Edges of the 7-day connection window
--   1001: joins board 10 on day 1 where 2001 already is; 2001 leaves on day 3 → connected on day 1
--   1002: creates board 11, but the other member joins on day 8 (outside the window) → not connected
--   1003: joins board 12, but 2003 left before 1003 arrived → not connected
--   1004: less than 7 days since signup → not included
--   2001: signed up Feb 1, so its window (2/1–2/8) ends before 1001 signs up → not connected
INSERT INTO snapshot_meta (source_cutoff_at, collection_started_at, collection_finished_at, spec_hash)
VALUES ('2024-03-20 00:00:00.000000', '2024-03-20 00:00:00.000000', '2024-03-20 00:00:01.000000', 'test');
INSERT INTO snapshot_params VALUES ('cohort_start', '2024-01-01 00:00:00.000000'), ('calendar_start', '2024-02-26 00:00:00.000000');

INSERT INTO r_member VALUES
  (1001, '2024-03-01 09:00:00.000000', NULL, 'KR'),
  (1002, '2024-03-01 09:00:00.000000', NULL, 'KR'),
  (1003, '2024-03-01 09:00:00.000000', NULL, 'KR'),
  (1004, '2024-03-15 09:00:00.000000', NULL, 'KR'),
  (2001, '2024-02-01 09:00:00.000000', NULL, 'KR'),
  (2002, '2024-02-01 09:00:00.000000', NULL, 'KR'),
  (2003, '2024-02-01 09:00:00.000000', NULL, 'KR');
INSERT INTO r_board VALUES
  (10, '2024-02-01 10:00:00.000000', NULL, 2001),
  (11, '2024-03-01 10:00:00.000000', NULL, 1002),
  (12, '2024-02-01 10:00:00.000000', NULL, 2003);
INSERT INTO r_board_member VALUES
  (10, 2001, '2024-02-01 10:00:00.000000', '2024-03-03 09:00:00.000000'),
  (10, 1001, '2024-03-02 09:00:00.000000', NULL),
  (11, 1002, '2024-03-01 10:00:00.000000', NULL),
  (11, 2002, '2024-03-09 09:00:00.000000', NULL),
  (12, 2003, '2024-02-01 10:00:00.000000', '2024-03-01 12:00:00.000000'),
  (12, 1003, '2024-03-02 09:00:00.000000', NULL);
