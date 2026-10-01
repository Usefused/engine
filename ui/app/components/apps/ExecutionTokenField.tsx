import { CopyValue } from "~/components/CopyValue";

/** Presents one-time runtime credentials with the same compact copy affordance as invocation URLs. */
export function ExecutionTokenField({ token, onCopy }: { token: string; copied: boolean; onCopy: () => void }) {
  return <CopyValue value={token} label="execution token" className="mt-3" onCopied={onCopy} />;
}
