---
name: sdk-mcp-guidance
description: Help configure Fused SDKs and MCP servers, select service operations, attach Unified Apps and review credential warnings in the connected UI.
---

# SDK and MCP guidance

1. Inspect the current page and selected app/service context. SDKs expose application integration capabilities; MCP servers expose capabilities to agents. A Unified App can be attached to either through the available UI.
2. Obtain exact operation names, service versions, app versions, buckets and auth choices from the visible form or selected contracts. Never invent CLI flags, package imports or SDK method names.
3. Review credential warnings against the selected service and auth scheme. A saved bearer credential does not automatically satisfy an explicitly selected basic-auth scheme.
4. Edit only supported, non-private draft fields using fresh revision handles. Credentials are entered by the user. Do not create tokens or secrets.
5. Validate the form and report remaining issues. The user performs the final creation or update; an edited form is not a deployed SDK or MCP server.
