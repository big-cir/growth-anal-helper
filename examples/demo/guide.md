# Demo workspace guide: a fictional community board app

Members create or join group boards, write posts on boards, and comment on or like other members' posts. All data is fictional and generated from deterministic random numbers.

## Terms

- **Member**: a user who signed up. Leaving sets `deleted_at`.
- **Board (group)**: created by a member (`owner_member_id`) and joined by others. Boards can be deleted.
- **Posts, comments, replies, likes**: writing activity on a board. A reply is a comment on a comment (`is_nested`).
- **Connection**: being a member of the same board as another member at the same time.

## Rules (already applied in the derived tables)

- Signup weeks start on Monday (`signup_week`, `YYYY-MM-DD`).
- Cohort metrics only use members who signed up on or after `cohort_start` (`d_member` already filters them).
- A membership interval is `[joined_at, ended_at)`: it ends at the earlier of leaving the board and the board's deletion (no end if neither).
- Deleted activity still counts as activity at the time it happened (`d_activity.source_deleted` marks it).
- The first-week funnel (`d_member_first_week`) only has members at least 7 days past signup, and each step is set only for members who reached the previous one (otherwise NULL).
- This demo has no undecidable states (join times are exact). The funnel's `unknown` column is still 0.

## Defaults

- First-week window: 7 days after signup.
- Retention: Wn = at least one activity in days [7n, 7n+7) after signup; the denominator only has members whose window has fully passed (observable). Members who left stay in the denominator.
- Activity: any post, comment, reply or like (all of `d_activity`). For posts only, use `kind = 'post'`.
- "Last N weeks" means the last N signup weeks whose observation window has ended.

## Columns

| Column | Description |
|---|---|
| `d_member.signup_at` | Signup time |
| `d_member.signup_week` | Signup week (Monday, YYYY-MM-DD) |
| `d_activity.kind` | post / reply / nested_reply / reaction |
| `d_activity.at` | Activity time |
| `d_activity.source_deleted` | 1 if the source row was deleted |
| `d_membership.joined_at` | Time the member joined the board (creation time for the creator) |
| `d_membership.ended_at` | End of membership (earlier of leaving and board deletion; NULL if neither) |
| `d_membership.is_owner` | 1 if the member created the board |
| `d_member_first_week.week_end` | Signup + 7 days |
| `d_member_first_week.board_state` | Joined a board within 7 days: reached / not |
| `d_member_first_week.connected_state` | Shared a board with another member within 7 days: reached / not (NULL if the previous step was not reached) |
| `d_member_first_week.post_state` | First post within 7 days, after connecting |
| `d_member_first_week.received_state` | After the first post, another member commented on or liked one of their posts written since then |
| `d_member_activity_week.life_week` | Week number since signup (from 0) |
| `d_member_activity_week.any_cnt` | Activities that week |
| `d_member_activity_week.post_cnt` | Posts that week |
| `d_calendar_week.week_start` | Week list made by the engine (Monday 00:00) |
