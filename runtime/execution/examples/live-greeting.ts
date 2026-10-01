import * as z from "zod/mini";
import { buildUnifiedApp } from "@fused/unified-app";
import { fused } from "@fused/operations";

export default buildUnifiedApp({
  input: z.object({ name: z.string() }),
  output: z.object({ greeting: z.string() }),
  fetch: { searchable: ["name"] },
  // Route the selected operation through Engine before recording searchable data.
  async execute({ input }) {
    const result = await fused.greeting.greet({ name: input.name });
    await fused.db.set({ name: input.name });
    return { greeting: result.greeting };
  },
});
