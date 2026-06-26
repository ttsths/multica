/**
 * Prefetch hook fired when the user taps an issue row in any list. The row
 * already carries the full `Issue` object (the list payload selects the
 * complete column set, including `description`), so instead of waiting for
 * the detail screen to fire `GET /api/issues/:id`, we synchronously drop
 * that row into the detail cache with `setQueryData`. The detail screen's
 * `issueDetailOptions(..., qc)` `initialData` factory then finds it
 * immediately and the first paint is near-zero latency — the network
 * refetch happens in the background and reconciles behind the seeded UI.
 *
 * Timeline + attachments can't be seeded (no row data for them), so we fire
 * `prefetchQuery` for those — the requests start now, in parallel with the
 * navigation push, so by the time the screen mounts the responses are
 * usually already cached. This is the list → detail "seamless" handoff
 * called out as 方案 F in the loading-optimization plan.
 *
 * WS handlers in `use-issue-realtime.ts` keep all three caches fresh once
 * the screen is open, and the 60s `staleTime` means re-entering the same
 * issue won't refetch at all — so the prefetch here is only paying for the
 * first entry, not every tap.
 */
import { useCallback } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { Issue } from "@multica/core/types";
import {
  issueAttachmentsOptions,
  issueKeys,
  issueTimelineOptions,
} from "@/data/queries/issues";
import { useWorkspaceStore } from "@/data/workspace-store";

export function usePrefetchIssue() {
  const qc = useQueryClient();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);

  return useCallback(
    (issue: Pick<Issue, "id" | "workspace_id">) => {
      const scope = wsId ?? issue.workspace_id;
      if (!scope || !issue?.id) return;
      // Seed the detail cache from the list row we just tapped. We force
      // `updatedAt` to the epoch so React Query treats the seed as stale
      // and fires a background refetch (`GET /api/issues/:id`) to
      // reconcile any drift since the list was fetched — without ever
      // blocking the first paint, which renders from this seed
      // immediately. Without this, `setQueryData` defaults `updatedAt` to
      // `now` and the 60s staleTime would suppress the reconciliation
      // fetch entirely.
      qc.setQueryData(issueKeys.detail(scope, issue.id), issue, {
        updatedAt: 0,
      });
      // Kick timeline + attachments off in parallel with navigation.
      void qc.prefetchQuery(issueTimelineOptions(scope, issue.id));
      void qc.prefetchQuery(issueAttachmentsOptions(scope, issue.id));
    },
    [qc, wsId],
  );
}
