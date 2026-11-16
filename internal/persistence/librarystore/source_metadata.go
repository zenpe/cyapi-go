package librarystore

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func mergeSourceMetadata(tx *gorm.DB) clause.Expr {
	old := "playlist_tracks_catalog.source_ids_json"
	incoming := "excluded.source_ids_json"
	if tx.Dialector.Name() == "postgres" {
		a := "COALESCE(" + old + ", '{}')::jsonb"
		b := "COALESCE(" + incoming + ", '{}')::jsonb"
		am := "COALESCE((" + a + ")->'lxMetadata', '{}'::jsonb)"
		bm := "COALESCE((" + b + ")->'lxMetadata', '{}'::jsonb)"
		aq := "COALESCE((" + am + ")->'qualities', '{}'::jsonb)"
		bq := "COALESCE((" + bm + ")->'qualities', '{}'::jsonb)"
		return gorm.Expr("((" + a + " || " + b + ") || CASE WHEN (" + a + ") ? 'lxMetadata' OR (" + b + ") ? 'lxMetadata' THEN jsonb_build_object('lxMetadata', " + am + " || " + bm + " || CASE WHEN (" + am + ") ? 'qualities' OR (" + bm + ") ? 'qualities' THEN jsonb_build_object('qualities', " + aq + " || " + bq + ") ELSE '{}'::jsonb END) ELSE '{}'::jsonb END)::text")
	}
	return gorm.Expr("CASE WHEN " + incoming + " IS NULL THEN " + old + " ELSE json_patch(COALESCE(" + old + ", '{}'), " + incoming + ") END")
}
