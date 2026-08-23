CREATE TABLE project_profiles (
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9-]{0,62}$'),
  schema_version text NOT NULL CHECK (schema_version ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$'),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  draft_id uuid NOT NULL,
  artifact_id uuid NOT NULL,
  digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  status text NOT NULL CHECK (status IN ('active', 'archived')),
  created_by uuid NOT NULL REFERENCES users(id),
  updated_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, project_id, kind),
  FOREIGN KEY (project_id, workspace_id) REFERENCES projects(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (artifact_id, workspace_id, project_id) REFERENCES artifacts(id, workspace_id, project_id)
);

CREATE FUNCTION validate_project_profile_artifact() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  target_artifact_id uuid;
  profile_row project_profiles%ROWTYPE;
  artifact_row artifacts%ROWTYPE;
BEGIN
  IF TG_TABLE_NAME = 'project_profiles' THEN
    target_artifact_id := COALESCE(NEW.artifact_id, OLD.artifact_id);
  ELSE
    target_artifact_id := COALESCE(NEW.id, OLD.id);
  END IF;
  FOR profile_row IN SELECT * FROM project_profiles WHERE artifact_id = target_artifact_id LOOP
    SELECT * INTO artifact_row FROM artifacts WHERE id = profile_row.artifact_id AND workspace_id = profile_row.workspace_id AND project_id = profile_row.project_id;
    IF artifact_row.id IS NULL OR artifact_row.status <> 'ready' OR artifact_row.digest <> profile_row.digest THEN
      RAISE EXCEPTION 'Project profile Artifact is unavailable or digest-mismatched' USING ERRCODE = '23514';
    END IF;
  END LOOP;
  IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;

CREATE CONSTRAINT TRIGGER project_profile_artifact_from_profile
AFTER INSERT OR UPDATE ON project_profiles DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_project_profile_artifact();

CREATE CONSTRAINT TRIGGER project_profile_artifact_from_artifact
AFTER UPDATE OR DELETE ON artifacts DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_project_profile_artifact();

GRANT SELECT, INSERT, UPDATE ON TABLE project_profiles TO blazn_runtime;
REVOKE DELETE ON TABLE project_profiles FROM blazn_runtime;
REVOKE ALL ON TABLE project_profiles FROM blazn_bootstrap;
REVOKE ALL ON FUNCTION validate_project_profile_artifact() FROM PUBLIC;
REVOKE ALL ON FUNCTION validate_project_profile_artifact() FROM blazn_bootstrap;
GRANT EXECUTE ON FUNCTION validate_project_profile_artifact() TO blazn_runtime;
