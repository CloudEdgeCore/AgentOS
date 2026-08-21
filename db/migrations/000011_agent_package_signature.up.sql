-- ADR-010: published agent versions carry the verified package signature
-- envelope so admission and audits can prove who signed exactly what. All
-- three fields are set together by publish admission, or none: a signed
-- package is atomic.
ALTER TABLE agent_versions
    ADD COLUMN package_key_id text,
    ADD COLUMN package_signature text,
    ADD COLUMN package_manifest_digest text;

ALTER TABLE agent_versions
    ADD CONSTRAINT agent_versions_package_signature_shape CHECK (
        (package_key_id IS NULL AND package_signature IS NULL AND package_manifest_digest IS NULL)
        OR (
            package_key_id IS NOT NULL AND package_signature IS NOT NULL AND package_manifest_digest IS NOT NULL
            AND package_key_id <> '' AND package_signature <> '' AND package_manifest_digest <> ''
        )
    );
