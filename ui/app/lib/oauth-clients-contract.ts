export const OAUTH_CLIENT_FIELDS = `
  id name client_id client_type redirect_uris allowed_scopes has_secret created_at revoked_at
`;

export const OAUTH_CLIENT_OPERATIONS = {
  list: `
    query OAuthClients {
      oauthClients { ${OAUTH_CLIENT_FIELDS} }
    }
  `,
  create: `
    mutation CreateOAuthClient($input: CreateOAuthClientInput!) {
      createOAuthClient(input: $input) {
        client { ${OAUTH_CLIENT_FIELDS} }
        client_secret
      }
    }
  `,
  revoke: `
    mutation RevokeOAuthClient($id: ID!) {
      revokeOAuthClient(id: $id)
    }
  `,
  // Fetches the server's built-in scope catalog (value + label) so the scope
  // picker never hardcodes a copy that could drift from the source of truth.
  scopeCatalog: `
    query OAuthScopeCatalog {
      oauthScopeCatalog { value label }
    }
  `,
} as const;
