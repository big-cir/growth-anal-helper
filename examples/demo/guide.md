# 데모 워크스페이스 설명서: 가상 커뮤니티 게시판

회원이 소모임 게시판을 만들거나 가입하고, 게시판에 글을 쓰고, 다른 회원의 글에 댓글·공감을 남기는 서비스다. 모든 데이터는 결정적 난수로 만든 가상 데이터다.

## 용어

- **회원**: 가입한 사용자. 탈퇴하면 `deleted_at`이 생긴다.
- **게시판(소모임)**: 회원이 만들고 다른 회원이 가입한다. 만든 사람은 `owner_member_id`. 게시판은 삭제될 수 있다.
- **글·댓글·답글·공감**: 게시판 안의 쓰기 활동. 답글은 댓글에 단 댓글(`is_nested`).
- **연결**: 같은 게시판에 다른 회원과 동시에 멤버로 있는 상태.

## 해석 규칙 (파생 테이블에 이미 들어 있음)

- 가입 주는 월요일 시작(`signup_week`, `YYYY-MM-DD`).
- 코호트 지표는 `cohort_start` 이후 가입자만 쓴다(`d_member`가 이미 거른다).
- 멤버십 구간은 `[joined_at, ended_at)`: 떠난 시각·게시판 삭제 중 이른 값이 끝(없으면 끝 없음).
- 삭제된 활동도 "그때 활동했다"로 센다(`d_activity.source_deleted`로 구분).
- 첫 주 퍼널(`d_member_first_week`)은 가입 후 7일이 지난 회원만 있고, 각 단계는 앞 단계 도달자에게만 매긴다(아니면 NULL).
- 판정 불가 상태는 이 데모에 없다(참여 시각이 정확하다). 그래도 퍼널의 `unknown` 칸은 0으로 둔다.

## 기본값

- 첫 주 지표의 창: 가입 후 7일.
- 리텐션: Wn = 가입 후 [7n, 7n+7)일에 활동 1건 이상, 분모는 그 구간이 다 지난 회원만(관측 가능 조건). 탈퇴 회원은 분모에 남긴다.
- 활동 기준: 글·댓글·답글·공감 중 하나(`d_activity` 전체). 글만 볼 때는 `kind = 'post'`.
- "최근 N주"는 관측이 끝난 가입 주 기준.

## 칸 설명

| 칸 | 설명 |
|---|---|
| `d_member.signup_at` | 가입 시각 |
| `d_member.signup_week` | 가입 주(월요일, YYYY-MM-DD) |
| `d_activity.kind` | post / reply / nested_reply / reaction |
| `d_activity.at` | 활동 시각 |
| `d_activity.source_deleted` | 원본 행이 삭제됐으면 1 |
| `d_membership.joined_at` | 게시판 가입 시각(만든 사람은 만든 시각) |
| `d_membership.ended_at` | 멤버였던 끝(떠남·게시판 삭제 중 이른 값, 없으면 NULL) |
| `d_membership.is_owner` | 게시판을 만든 사람이면 1 |
| `d_member_first_week.week_end` | 가입 + 7일 |
| `d_member_first_week.board_state` | 7일 안에 게시판 참여: reached / not |
| `d_member_first_week.connected_state` | 7일 안에 다른 회원과 같은 게시판: reached / not (앞 단계 미도달이면 NULL) |
| `d_member_first_week.post_state` | 연결 뒤 7일 안의 첫 글 |
| `d_member_first_week.received_state` | 첫 글 뒤, 첫 글 이후 쓴 내 글에 다른 회원의 댓글·공감 |
| `d_member_activity_week.life_week` | 가입 후 몇 번째 주(0부터) |
| `d_member_activity_week.any_cnt` | 그 주 활동 수 |
| `d_member_activity_week.post_cnt` | 그 주 글 수 |
| `d_calendar_week.week_start` | 엔진이 만든 주 목록(월요일 00:00) |
