-- Demo derived tables. Date arithmetic uses strftime(…) || substr(t, 20) to keep the 6-digit fraction.

CREATE TABLE d_as_of AS
SELECT source_cutoff_at AS as_of FROM snapshot_meta;

-- Members and their signup week (Monday)
CREATE TABLE d_member AS
SELECT
  m.id AS member_id,
  m.created_at AS signup_at,
  date(m.created_at, 'weekday 0', '-6 days') AS signup_week,
  m.deleted_at,
  m.country
FROM r_member m
WHERE m.created_at >= (SELECT value FROM snapshot_params WHERE key = 'cohort_start');
CREATE INDEX d_member_signup ON d_member(signup_at);

-- Writing activity (deleted rows included)
CREATE TABLE d_activity AS
SELECT member_id, board_id, 'post' AS kind, created_at AS at, (deleted_at IS NOT NULL) AS source_deleted, id AS post_id
FROM r_post
UNION ALL
SELECT x.member_id, p.board_id, CASE WHEN x.is_nested THEN 'nested_reply' ELSE 'reply' END, x.created_at, (x.deleted_at IS NOT NULL), x.post_id
FROM r_reply x JOIN r_post p ON p.id = x.post_id
UNION ALL
SELECT x.member_id, p.board_id, 'reaction', x.created_at, 0, x.post_id
FROM r_reaction x JOIN r_post p ON p.id = x.post_id;
CREATE INDEX d_activity_member_at ON d_activity(member_id, at);
CREATE INDEX d_activity_board ON d_activity(board_id, member_id, at);

-- Membership intervals [joined_at, ended_at)
CREATE TABLE d_membership AS
SELECT
  bm.board_id, bm.member_id, bm.joined_at,
  CASE
    WHEN bm.left_at IS NULL THEN b.deleted_at
    WHEN b.deleted_at IS NULL THEN bm.left_at
    ELSE min(bm.left_at, b.deleted_at)
  END AS ended_at,
  (b.owner_member_id = bm.member_id) AS is_owner
FROM r_board_member bm
JOIN r_board b ON b.id = bm.board_id;
CREATE INDEX d_membership_member ON d_membership(member_id, joined_at);
CREATE INDEX d_membership_board ON d_membership(board_id, joined_at);

-- First-week funnel (members at least 7 days past signup). NULL if the previous step was not reached.
-- Joined a board → connected with another member → first post → got a reaction on their post
CREATE TABLE d_member_first_week AS
WITH base AS (
  SELECT
    m.member_id, m.signup_at, m.signup_week,
    strftime('%Y-%m-%d %H:%M:%S', m.signup_at, '+7 days') || substr(m.signup_at, 20) AS week_end
  FROM d_member m
  WHERE strftime('%Y-%m-%d %H:%M:%S', m.signup_at, '+7 days') || substr(m.signup_at, 20) <= (SELECT as_of FROM d_as_of)
),
joined AS (
  SELECT b.member_id, min(max(ms.joined_at, b.signup_at)) AS joined_at
  FROM base b
  JOIN d_membership ms ON ms.member_id = b.member_id
  WHERE ms.joined_at < b.week_end
    AND (ms.ended_at IS NULL OR ms.ended_at > max(ms.joined_at, b.signup_at))
  GROUP BY b.member_id
),
connected AS (
  SELECT b.member_id, min(max(u.joined_at, o.joined_at, b.signup_at)) AS connected_at
  FROM base b
  JOIN d_membership u ON u.member_id = b.member_id
  JOIN d_membership o ON o.board_id = u.board_id AND o.member_id <> u.member_id
  WHERE max(u.joined_at, o.joined_at, b.signup_at) < b.week_end
    AND (u.ended_at IS NULL OR u.ended_at > max(u.joined_at, o.joined_at, b.signup_at))
    AND (o.ended_at IS NULL OR o.ended_at > max(u.joined_at, o.joined_at, b.signup_at))
  GROUP BY b.member_id
),
first_post AS (
  SELECT b.member_id, min(a.at) AS first_post_at
  FROM base b
  JOIN connected c ON c.member_id = b.member_id
  JOIN d_activity a ON a.member_id = b.member_id AND a.kind = 'post'
  WHERE a.at >= c.connected_at AND a.at < b.week_end
  GROUP BY b.member_id
),
received AS (
  SELECT b.member_id, min(x.at) AS received_at
  FROM base b
  JOIN first_post f ON f.member_id = b.member_id
  JOIN r_post p ON p.member_id = b.member_id AND p.created_at >= f.first_post_at
  JOIN d_activity x ON x.post_id = p.id AND x.kind IN ('reply', 'nested_reply', 'reaction') AND x.member_id <> b.member_id
  WHERE x.at >= f.first_post_at AND x.at < b.week_end
  GROUP BY b.member_id
)
SELECT
  b.member_id, b.signup_at, b.signup_week, b.week_end,
  CASE WHEN j.joined_at IS NOT NULL THEN 'reached' ELSE 'not' END AS board_state,
  j.joined_at AS board_at,
  CASE WHEN j.joined_at IS NULL THEN NULL WHEN c.connected_at IS NOT NULL THEN 'reached' ELSE 'not' END AS connected_state,
  c.connected_at,
  CASE WHEN c.connected_at IS NULL THEN NULL WHEN f.first_post_at IS NOT NULL THEN 'reached' ELSE 'not' END AS post_state,
  f.first_post_at,
  CASE WHEN f.first_post_at IS NULL THEN NULL WHEN r.received_at IS NOT NULL THEN 'reached' ELSE 'not' END AS received_state,
  r.received_at
FROM base b
LEFT JOIN joined j ON j.member_id = b.member_id
LEFT JOIN connected c ON c.member_id = b.member_id
LEFT JOIN first_post f ON f.member_id = b.member_id
LEFT JOIN received r ON r.member_id = b.member_id;
CREATE INDEX d_member_first_week_week ON d_member_first_week(signup_week);

-- Activity by week since signup
CREATE TABLE d_member_activity_week AS
SELECT
  a.member_id,
  CAST((julianday(a.at) - julianday(m.signup_at)) / 7 AS INTEGER) AS life_week,
  count(*) AS any_cnt,
  sum(a.kind = 'post') AS post_cnt
FROM d_activity a
JOIN d_member m ON m.member_id = a.member_id
WHERE a.at >= m.signup_at
GROUP BY a.member_id, life_week;
CREATE INDEX d_member_activity_week_member ON d_member_activity_week(member_id, life_week);
