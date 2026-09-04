package api

import (
	"fmt"

	"github.com/google/uuid"
)

// appRuntimeSetupOverview explains app tokens and provider connections inside
// the exported contract without relying on an SDK package or separate README.
func appRuntimeSetupOverview(appID uuid.UUID) string {
	return fmt.Sprintf(`This contract belongs to one immutable Fused app version and remains available when no SDK package is built.

Use your Fused CLI login to manage the app and export this document. Runtime calls require an app execution token in the Authorization Bearer header. Provider credentials belong in the app's bucket.

For OAuth/OIDC, store the application's client ID and client secret if they are missing, then run `+"`fused-cli workspace service connect`"+` for each service and user. Open the returned URL and complete consent. Use the same stable user reference in `+"`selector.end_user_ref`"+` when calling a physical operation. Unified operations use service-keyed `+"`selectors`"+` and explicit `+"`targets`"+`, as described by their request schema.

An app can be created before credentials or connections are ready. `+"`authentication_required`"+` means the app token is missing; `+"`connection_required`"+` means the selected user needs a provider connection. If the app is configured with multiple provider resources, pass the selected `+"`resource_id`"+` too.

Export this exact contract with `+"`fused-cli api openapi %s --out app.openapi.yaml`"+`. Fill the selected operation's required input from its schema before executing it. Execution tokens and provider credentials are never embedded in this document.
`, appID)
}
