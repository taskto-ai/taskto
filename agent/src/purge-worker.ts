// The worker thread of a purge (purge.ts): does one job and says how it ended. It runs nothing
// else, and never reads a secret: the job names the database and the conversation.
import { parentPort, workerData } from 'node:worker_threads'
import { type PurgeJob, type PurgeOutcome, runPurgeJob } from './purge.js'
import { WalBusyError } from './wal.js'

let outcome: PurgeOutcome
try {
  runPurgeJob(workerData as PurgeJob)
  outcome = { ok: true }
} catch (error) {
  outcome = { ok: false, busy: error instanceof WalBusyError, message: (error as Error).message ?? String(error) }
}
parentPort?.postMessage(outcome)
