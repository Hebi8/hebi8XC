package sessions

// A separate table keeps the declaration additive and rollback-safe. Any old
// writer changing content invalidates it; new writers record it after content
// in the same transaction. Generation/revision also bind it to the actual copy.
const copyStructureSchema = `
CREATE TABLE IF NOT EXISTS copy_structure (
 node TEXT NOT NULL,key TEXT NOT NULL,gen INTEGER NOT NULL,rev INTEGER NOT NULL,level INTEGER,
 PRIMARY KEY(node,key)
) WITHOUT ROWID;
CREATE TRIGGER IF NOT EXISTS copy_structure_insert AFTER INSERT ON content BEGIN
 DELETE FROM copy_structure WHERE node=new.node AND key=new.key; END;
CREATE TRIGGER IF NOT EXISTS copy_structure_update AFTER UPDATE ON content BEGIN
 DELETE FROM copy_structure WHERE node=old.node AND key=old.key; END;
CREATE TRIGGER IF NOT EXISTS copy_structure_delete AFTER DELETE ON content BEGIN
 DELETE FROM copy_structure WHERE node=old.node AND key=old.key; END;
`

// User messages do not carry calls. Their missing transcript annotations must
// not lower the page's declaration of tool/file structure.
func structureLevel(sess Session, msgs []Message) *int {
	level := sess.Structure
	found := false
	for _, m := range msgs {
		if m.Role != "user" {
			if !found {
				level = m.Structure
				found = true
			} else {
				level = min(level, m.Structure)
			}
		}
	}
	return &level
}
