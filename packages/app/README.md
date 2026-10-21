# @openseal/app

What an OpenSeal app imports to work with its Agent: start runs, follow their
progress, and read data that a routine keeps fresh.

```tsx
import { useData, useRun } from "@openseal/app/react";

export default function App() {
  const { start, state, running } = useRun();
  const pipeline = useData<{ stage: string; value: number }[]>("pipeline_by_stage");
  return <>
    <button disabled={running} onClick={() => start({ message: "Research NVDA vs AMD", values: { ticker: "NVDA", compare: "AMD" } })}>Run</button>
    {state?.status === "completed" && <p>{state.summary}</p>}
    {pipeline.data?.map(row => <p key={row.stage}>{row.stage}: {row.value}</p>)}
  </>;
}
```

Without React: `runAgent`, `getData` and `watchData` from `@openseal/app`.

## Where it runs

- **In OpenSeal's canvas** nothing needs configuring. The app runs in a
  sandboxed frame and talks to the page through versioned messages
  (`src/protocol.ts`); the host checks every request against the app's
  permissions. A request from the app appears in the chat as the user's turn.
- **Downloaded and self-hosted**, call `configure({ baseUrl, appId, appKey })`
  once. The app key can only start this app's Agent and read this app's data,
  is subject to its limits, and can be revoked in OpenSeal at any time.

## Errors

Every failure is an `OpenSealAppError` with a `code`: `not_available`,
`not_allowed`, `limit_reached`, `not_found`, `invalid_request` or
`unavailable`.

## Develop

```sh
npm run typecheck
npm test   # node --test, Node 22.18 or later
```
