DROP TABLE IF EXISTS treasury_tenant_custom_wallets;
DROP TABLE IF EXISTS treasury_hd_account_counter;
DROP TABLE IF EXISTS treasury_tenant_hd_accounts;

ALTER TABLE treasury_address_reservations DROP COLUMN tenant_id;

ALTER TABLE derived_addresses DROP CONSTRAINT derived_addresses_chain_tenant_index_key;
ALTER TABLE derived_addresses DROP COLUMN tenant_id;
ALTER TABLE derived_addresses ADD CONSTRAINT derived_addresses_chain_derivation_index_key UNIQUE (chain, derivation_index);

ALTER TABLE hd_wallet_indices DROP CONSTRAINT hd_wallet_indices_pkey;
ALTER TABLE hd_wallet_indices DROP COLUMN tenant_id;
ALTER TABLE hd_wallet_indices ADD PRIMARY KEY (chain);
