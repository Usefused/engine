export type ServiceAuthOption = {
  id: string;
  label: string;
  auth_type: string;
  credential_type: string;
  key_name: string;
  key_prefix: string;
  required_fields: string[];
  supports_connected_users: boolean;
};
