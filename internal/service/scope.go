package service

import (
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// ApplyFolderScope membatasi query app_folders sesuai cakupan Tim/Branch user (Admin: semua folder). Dipakai menu
// Aplikasi, upload, dan tool Agentic AI agar aturan aksesnya selalu sama.
func ApplyFolderScope(q *gorm.DB, user models.User) *gorm.DB {
	if user.IsAdmin() {
		return q
	}
	switch {
	case user.TeamID != nil && user.BranchID != nil:
		return q.Where("assigned_team_id = ? AND (assigned_branch_id IS NULL OR assigned_branch_id = '' OR assigned_branch_id = ?)", *user.TeamID, *user.BranchID)
	case user.TeamID != nil:
		return q.Where("assigned_team_id = ?", *user.TeamID)
	default:
		return q.Where("1 = 0")
	}
}

// CanAccessFolder adalah versi in-memory dari ApplyFolderScope untuk satu folder.
func CanAccessFolder(user models.User, f models.AppFolder) bool {
	if user.IsAdmin() {
		return true
	}
	if user.TeamID == nil || f.AssignedTeamID != *user.TeamID {
		return false
	}
	if user.BranchID == nil {
		return true
	}
	return f.AssignedBranchID == nil || *f.AssignedBranchID == "" || *f.AssignedBranchID == *user.BranchID
}
