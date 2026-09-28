declare module "@fused/operations" {
export type Operation0Input = { "name": string; };
export type Operation0Output = { "greeting": string; };
export const services: { readonly "greeting": { readonly "greet": (input: Operation0Input) => Promise<Operation0Output>; }; };
}
