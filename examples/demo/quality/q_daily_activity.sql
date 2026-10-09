-- Signups, posts and comments per day
WITH d AS (
  SELECT date(created_at) AS day, 'signups' AS k FROM r_member
  UNION ALL SELECT date(created_at), 'posts' FROM r_post
  UNION ALL SELECT date(created_at), 'replies' FROM r_reply
)
SELECT day, sum(k = 'signups') AS signups, sum(k = 'posts') AS posts, sum(k = 'replies') AS replies
FROM d
WHERE day >= date(:as_of, '-90 days')
GROUP BY day
ORDER BY day
