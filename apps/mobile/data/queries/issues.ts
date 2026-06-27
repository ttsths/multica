/**
 * Issue queries — workspace-wide list, single-issue detail, timeline.
 * Mobile-owned; mirrors a strict subset of packages/core/issues/queries.ts.
 *
 * Query keys live in ./issue-keys so detail / timeline / list / myList all
 * sit under the `issues/<wsId>` prefix — WS handlers can invalidate the
 * whole subtree with one call when needed.
 */
import { queryOptions, type QueryClient } from "@tanstack/react-query";
import type { Issue } from "@multica/core/types";
import { api } from "@/data/api";
import { issueKeys } from "./issue-keys";

export { issueKeys } from "./issue-keys";

/**
 * How long detail / timeline / attachment reads stay fresh before React
 * Query will refetch on re-focus. WS events still invalidate on arrival
 * (see `use-issue-realtime.ts`), so this only kills the redundant
 * refetch that fires every time the user re-enters an issue they were
 * just looking at — the main source of the "noticeable loading delay"
 * versus web/desktop, which keeps these responses cached much longer.
 */
const ISSUE_STALE_TIME_MS = 60_000;

/**
 * Best-effort first-paint seed: walk every cached issue list under the
 * current workspace (workspace-wide `list`, `my/list`, any future list
 * variant) and return the matching issue if one exists. The server's
 * `ListIssues` / `MyIssues` queries select the full `Issue` column set
 * (including `description` — see server/pkg/db/queries/issue.sql), so the
 * cached row is structurally complete and safe to render with before the
 * `GET /api/issues/:id` round-trip lands.
 *
 * Returns `undefined` when nothing is cached so React Query treats the
 * detail query as genuinely pending (full-screen spinner) — that only
 * happens on a true cold start (deep link, fresh launch) where there is
 * no list cache to seed from.
 */
function findCachedIssue(
  qc: QueryClient,
  wsId: string | null,
  id: string,
): Issue | undefined {
  if (!wsId || !id) return undefined;
  const entries = qc.getQueriesData<unknown>({
    queryKey: issueKeys.all(wsId),
  });
  for (const [, data] of entries) {
    if (!data) continue;
    // List-shaped caches are `Issue[]` (mobile strips `.issues` at the
    // fetch boundary); detail itself is a bare `Issue` and timeline is
    // `TimelineEntry[]`, both skipped by the array guard.
    const arr = Array.isArray(data) ? (data as Issue[]) : undefined;
    if (!arr) continue;
    const hit = arr.find((i) => i?.id === id);
    if (hit) return hit;
  }
  return undefined;
}

/**
 * Workspace-wide issue list. Backend filters by `X-Workspace-Slug` header
 * (root CLAUDE.md "All queries filter by workspace_id"), so we pass an
 * empty params object — server returns every issue the user is allowed to
 * see in the current workspace.
 *
 * Cache shape: flat `Issue[]` (we strip `.issues` from the response) so
 * the WS updaters can patch this list with the same shape as
 * myIssueListOptions. Pagination is deferred — web's `IssuesPage` also
 * fetches all in one shot today (`packages/views/issues/components/
 * issues-page.tsx:30`).
 */
export const issueListOptions = (wsId: string | null) =>
  queryOptions({
    queryKey: issueKeys.list(wsId),
    queryFn: async ({ signal }) => {
      const res = await api.listIssues({}, { signal });
      return res.issues;
    },
    enabled: !!wsId,
  });

/**
 * Detail query for a single issue. Seeds `initialData` from any cached
 * issue list under the same workspace so the first paint is near-zero
 * latency — the list row the user just tapped already carries the full
 * `Issue` (see `findCachedIssue`). The seed is treated as stale, so React
 * Query refetches `GET /api/issues/:id` in the background to reconcile
 * any field the list payload omits and to pick up edits made since the
 * list was fetched. WS events (`useIssueRealtime`) also patch this cache,
 * so a 60s `staleTime` is safe — repeated navigation back into the same
 * issue skips the refetch entirely while realtime keeps it fresh.
 *
 * Pass the active `QueryClient` (from `useQueryClient()`) to enable
 * seeding; omit it on call sites that don't need the seed.
 */
export const issueDetailOptions = (
  wsId: string | null,
  id: string,
  qc?: QueryClient,
) =>
  queryOptions({
    queryKey: issueKeys.detail(wsId, id),
    queryFn: ({ signal }) => api.getIssue(id, { signal }),
    enabled: !!wsId && !!id,
    staleTime: ISSUE_STALE_TIME_MS,
    initialData: qc
      ? () => findCachedIssue(qc, wsId, id)
      : undefined,
  });

/**
 * Single query over the full issue timeline (ASC, oldest first). Mirrors
 * web's `issueTimelineOptions` post-#2322 — server returns the whole list
 * in one shot, client-side pagination was deleted.
 */
export const issueTimelineOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: issueKeys.timeline(wsId, id),
    queryFn: ({ signal }) => api.listTimeline(id, { signal }),
    enabled: !!wsId && !!id,
    // WS events (comment:created, activity, reactions) invalidate this
    // query on arrival, so a 60s staleTime only skips the redundant
    // refetch when re-entering an already-current issue — timeline never
    // goes visually stale while the screen is open.
    staleTime: ISSUE_STALE_TIME_MS,
  });

/**
 * Currently-running tasks for an issue. WS events (task:queued/dispatch/
 * progress/completed/failed/cancelled) patch this cache directly via
 * `issue-ws-updaters.ts`, so refetches are rare in practice. The fetch is
 * still wired so the initial open + reconnect-invalidate path works.
 */
export const issueActiveTasksOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: issueKeys.activeTasks(wsId, id),
    queryFn: ({ signal }) => api.listActiveTasksForIssue(id, { signal }),
    enabled: !!wsId && !!id,
  });

/**
 * All tasks (any status) for an issue — drives the Runs sheet history
 * section. Same patching strategy as active tasks: WS moves entries between
 * the two caches without refetching.
 */
export const issueTasksOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: issueKeys.tasks(wsId, id),
    queryFn: ({ signal }) => api.listTasksByIssue(id, { signal }),
    enabled: !!wsId && !!id,
  });

/**
 * File attachments uploaded to this issue or any of its comments. The
 * mobile markdown renderer reads this list to resolve `mc://file/<id>`
 * URIs in image markdown to a real HTTPS `download_url` that iOS can
 * actually load — see `lib/markdown/markdown-image.tsx`.
 *
 * TanStack Query dedupes the request across concurrent callers, so it's
 * safe for both IssueDescription and CommentCard to fetch the same
 * issue's attachments — only one network request fires.
 */
export const issueAttachmentsOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: issueKeys.attachments(wsId, id),
    queryFn: ({ signal }) => api.listAttachments(id, { signal }),
    enabled: !!wsId && !!id,
    // Attachments rarely change while an issue is open; re-entering an
    // issue shouldn't re-block markdown image rendering with a fresh
    // round-trip. WS uploads still invalidate on arrival.
    staleTime: ISSUE_STALE_TIME_MS,
  });
