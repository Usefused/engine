# Pinned agent runtime

The vendored Harnest 0.23.0 wheel comes from the existing Threadify Engine runtime.
It includes the authenticated frontend-tool continuation fixes used by that agent.
The accompanying HARNEST-LICENSE applies to the wheel.

SHA-256: `b3f7d6262e761b0df4fefaf8339e6cf3cd9c9fbca4525fc157b64479567ac909`

CI builds the compiler from Harnest revision
`553638d5a12770c0bc9f777b4d10e18a73a19c05`, embedding this same wheel.
`harnest-runtime.lock` pins the runtime dependency hashes. Do not replace the
wheel without rebuilding and testing the compiled agent and native archives.
