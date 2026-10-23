// Keep a receipt of completed writes so a later failure does not require replay.
const progress = { drafts: [], skipped: [], pending: null };
session.set("reminders", progress);
const invoices = await call("listInvoices", {});
for (const invoice of invoices) {
  // Paid and future invoices must never trigger customer lookups or reminders.
  if (invoice.status !== "open" || invoice.dueDate >= "2026-10-19") {
    progress.skipped.push(invoice.id);
    continue;
  }
  const customer = await call("getCustomer", { id: invoice.customerId });
  // Customer preferences override an otherwise eligible invoice.
  if (customer.doNotContact) {
    progress.skipped.push(invoice.id);
    continue;
  }
  // Record the target before sending so a lost response leaves an inspectable uncertainty.
  progress.pending = invoice.id;
  const draft = await call("createReminderDraft", {
    invoiceId: invoice.id,
    to: customer.email,
    amountCents: invoice.amountCents,
    currency: invoice.currency
  });
  // Only an acknowledged write may move from pending into the completed receipt.
  progress.drafts.push({ invoiceId: invoice.id, draftId: draft.id });
  progress.pending = null;
}
return progress;
