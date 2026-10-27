import { Link, useParams, type MetaFunction } from "@remix-run/react";
import { WorkspacePermissionGate } from "~/components/access/CurrentActorAccess";
import { BridgeProfilePage } from "~/components/bridges/BridgeProfilePage";

/** Give directly linked profiles their own browser identity. */
export const meta: MetaFunction = ({ params }) => [{ title: `${params.profileId} - Bridge profile - Fused` }];

/** Keep direct profile links behind the same read boundary as Bridge discovery. */
export default function BridgeProfileRoute() {
  const { pairingId, profileId } = useParams();
  // Missing route identity must never issue an unscoped catalogue request.
  if (!pairingId || !profileId) return <Link to="/integrations/bridges">Back to Bridges</Link>;
  return <WorkspacePermissionGate permission="bridge.read" area="Bridge profiles">
    <BridgeProfilePage key={`${pairingId}:${profileId}`} pairing={pairingId} profile={profileId} />
  </WorkspacePermissionGate>;
}
