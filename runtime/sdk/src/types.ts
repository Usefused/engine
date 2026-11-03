/** Non-secret selectors are interpreted within the authenticated app's scope. */
export interface Selector { endUserRef?: string; authType?: string; authName?: string; resourceId?: string }
export interface OperationOptions { selector?: Selector; bucketId?: string; pagination?: { maxPages: number }; idempotencyKey?: string }
export interface ExecutionRequest extends OperationOptions { endpoint: string; input: unknown; signal?: AbortSignal }
export interface Transport { execute(request: ExecutionRequest): Promise<unknown> }
