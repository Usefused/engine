import { APIRequestError } from "./authorization-error.ts";

// waitForBilling leaves time for the account's webhook reconciliation transaction to release its lock.
function waitForBilling(milliseconds: number): Promise<void> {
  return new Promise((resolve) => { /* The timer only delays a safe canonical refresh; it never submits a purchase. */ setTimeout(resolve, milliseconds); });
}

// syncBillingAfterReturn retries only explicit lock contention; provider failures and payment actions are never replayed.
export async function syncBillingAfterReturn(sync: () => Promise<unknown>, wait = waitForBilling): Promise<void> {
  const delays = [500, 1000, 2000, 4000];
  for (let attempt = 0; ; attempt++) {
    try {
      await sync();
      return;
    } catch (error) {
      // A 409 proves another billing transaction holds the lock before reconciliation began; cap retries for persistent contention.
      if (!(error instanceof APIRequestError) || error.status !== 409 || attempt >= delays.length) throw error;
      await wait(delays[attempt]);
    }
  }
}
