declare module "@fused/operations" {
import { fused as runtime, type WorkspaceOperationRequest } from "@fused/unified-app";
export type Operation0Input = { "name": string; };
export type Operation0Output = { "greeting": string; };
export type OperationOptions = Pick<WorkspaceOperationRequest, "selector" | "pagination">;
type ExactServices = { readonly "greeting": { readonly "greet": (input: Operation0Input, options?: OperationOptions) => Promise<Operation0Output>; }; };
type SelectedServices = { "greeting": { "greet": ExactServices["greeting"]["greet"] } };
type BoundFused = ReturnType<typeof runtime.forUserRef> & SelectedServices;
export const services: ExactServices;
export const fused: Omit<typeof runtime, "forUserRef" | "forServiceUserRefs"> & SelectedServices & {
  forUserRef(ref: string): BoundFused;
  forServiceUserRefs(refs: Readonly<Record<string, string>>): BoundFused;
};
}
