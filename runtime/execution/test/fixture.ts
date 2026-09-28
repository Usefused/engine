import { z } from "zod";
import { buildExecutionApp, fused } from "@fused/execution";

export default buildExecutionApp({
  input: z.object({ name: z.string() }),
  output: z.object({ customerId: z.string() }),
  fetch: { searchable: ["customerId"] },
  // The fixture exercises the sandbox bridge and runtime output validation.
  async execute({ input }) {
    const customer = await fused.fetch({ service: "crm", operation: "create", input: { name: input.name }, selector: { environment: "prod" } });
    await fused.db.set({ customerId: (customer as { id: string }).id });
    return { customerId: (customer as { id: string }).id };
  },
});
