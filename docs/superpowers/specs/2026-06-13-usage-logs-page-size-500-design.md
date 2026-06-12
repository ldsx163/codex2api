# Usage Logs 500 Page Size Fix Design

Date: 2026-06-13

## Context

The admin usage log table can request page sizes of `10`, `20`, `50`, `100`, `200`, and `500`. The backend currently accepts only `page_size <= 200` in the `/api/admin/usage/logs` handler and in the shared paged database query. When the frontend requests `page=2&page_size=500`, the backend silently keeps the default page size of `20`. That changes the offset from the expected `500` to `20`, so the last page can show newer records instead of the oldest record.

The operations error log endpoint uses the same usage log paged database query and has its own page size filter limit. Its limit should be kept consistent with usage logs.

Per user request, `docs/API.md` is out of scope and will not be updated in this change.

## Goals

- Allow `page_size=500` for `/api/admin/usage/logs`.
- Allow `PageSize: 500` in `database.ListUsageLogsByTimeRangePaged`.
- Allow `page_size=500` in the operations error log pagination filter.
- Preserve existing invalid-value behavior: non-positive or over-limit page sizes fall back to the existing default of `20` rather than returning a new validation error.
- Add a regression test that fails if `page_size=500` is silently treated as `20`.

## Non-goals

- Do not change frontend pagination options.
- Do not change API error semantics for invalid `page_size` values.
- Do not update `docs/API.md` in this branch.
- Do not refactor unrelated pagination code.

## Design

### Handler pagination parsing

In `admin/handler.go`, update the `/api/admin/usage/logs` pagination parsing so a positive `page_size` up to `500` is accepted. Requests such as:

```text
/api/admin/usage/logs?page=2&page_size=500
```

will pass `PageSize: 500` into `database.UsageLogFilter`.

### Operations error log pagination filter

In `admin/handler.go`, update `parseOpsErrorLogFilter` to accept positive `page_size` values up to `500` when paging is enabled. This keeps `/api/admin/ops/errors` aligned with the usage logs endpoint because both use `ListUsageLogsByTimeRangePaged`.

### Database pagination guard

In `database/postgres.go`, update `ListUsageLogsByTimeRangePaged` so `PageSize: 500` is valid. The database layer will still normalize invalid input as it does today:

- `Page < 1` becomes `1`.
- `PageSize < 1` or `PageSize > 500` becomes `20`.

This preserves compatibility while fixing the frontend-supported `500` page size.

### Regression test

Add `TestGetUsageLogsAllowsFiveHundredPageSize` in `admin/handler_test.go`.

The test will:

1. Create a test admin database and handler.
2. Insert `501` usage logs in chronological order where `/v1/log-000` is the oldest and `/v1/log-500` is the newest.
3. Request the second page using `page=2&page_size=500` over a range that includes all inserted logs.
4. Assert the response is successful and contains:
   - `total == 501`
   - `len(logs) == 1`
   - the only returned log has endpoint `/v1/log-000`

If the backend still falls back to `20`, the query uses offset `20`, and this test will fail because the second page will not contain only the oldest record.

## Verification

Run the targeted regression test:

```powershell
go test ./admin -run TestGetUsageLogsAllowsFiveHundredPageSize -count=1 -v
```

Also run the package tests that cover the touched handler code:

```powershell
go test ./admin -count=1
```

## Risks

- Increasing the maximum page size can return more rows per request, but `500` already matches the frontend's supported selection and remains bounded.
- There are several existing hard-coded pagination limits. This change only updates the three limits needed for usage logs and operations error logs, avoiding broader unrelated behavior changes.
