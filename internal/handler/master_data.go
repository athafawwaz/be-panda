package handler

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// ─── Master Data Tim & Branch + Master Akses (Users) ─────────────────────────

var nonCodeChars = regexp.MustCompile(`[^A-Z0-9]+`)

// makeCode membentuk kode stabil dari nama, mis. "Tim Keamanan Siber" -> "TIM_KEAMANAN_SIBER".
// Kode tidak diubah saat rename agar tetap menjadi identifier yang konsisten.
func makeCode(name string) string {
	return strings.Trim(nonCodeChars.ReplaceAllString(strings.ToUpper(strings.TrimSpace(name)), "_"), "_")
}

func initialAdminBadge() string {
	if b := os.Getenv("INITIAL_ADMIN_BADGE"); b != "" {
		return b
	}
	return "2411249"
}

func requireAdmin(c *gin.Context, msg string) (models.User, bool) {
	u := getCurrentUser(c)
	if !u.IsAdmin() {
		c.JSON(http.StatusForbidden, gin.H{"error": msg})
		return u, false
	}
	return u, true
}

func (h *Handler) logMasterAction(actor models.User, actionType, detail string) {
	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        actor.BadgeNumber,
		UserName:           actor.FullName,
		UserRole:           actor.Role,
		TeamAndBranchLabel: formatUserTeamLabel(actor),
		ActionType:         actionType,
		ModuleSource:       "MASTER_ACCESS",
		DetailDescription:  detail,
		ActionTimestamp:    time.Now(),
	})
}

func nullable(p *string) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func trimmedOrNil(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

// resolveSegment memvalidasi Tim & Branch untuk role Pegawai TI. Role admin tidak terikat segmen.
func (h *Handler) resolveSegment(role string, teamID, branchID *string) (*string, *string, string) {
	if role != models.RolePegawaiTI {
		return nil, nil, ""
	}
	teamID, branchID = trimmedOrNil(teamID), trimmedOrNil(branchID)
	if teamID == nil {
		return nil, nil, "Pegawai TI wajib memiliki Tim"
	}
	var team models.MasterTeam
	if err := h.DB.First(&team, "id = ?", *teamID).Error; err != nil {
		return nil, nil, "Tim tidak ditemukan"
	}
	if branchID != nil {
		var branch models.MasterBranch
		if err := h.DB.First(&branch, "id = ? AND team_id = ?", *branchID, team.ID).Error; err != nil {
			return nil, nil, "Branch tidak ditemukan pada tim yang dipilih"
		}
	}
	return teamID, branchID, ""
}

// countUsage menghitung pemakaian Tim/Branch agar penghapusan tidak meninggalkan data yatim.
func (h *Handler) countUsage(userCol, folderCol, docCol, id string) (users, folders, docs int64) {
	h.DB.Model(&models.User{}).Where(userCol+" = ?", id).Count(&users)
	h.DB.Model(&models.AppFolder{}).Where(folderCol+" = ?", id).Count(&folders)
	h.DB.Model(&models.Document{}).Where(docCol+" = ?", id).Count(&docs)
	return
}

func usageMessage(subject string, users, folders, docs int64) string {
	var parts []string
	if users > 0 {
		parts = append(parts, fmt.Sprintf("%d pengguna", users))
	}
	if folders > 0 {
		parts = append(parts, fmt.Sprintf("%d folder", folders))
	}
	if docs > 0 {
		parts = append(parts, fmt.Sprintf("%d dokumen", docs))
	}
	return fmt.Sprintf("%s masih digunakan oleh %s. Pindahkan data tersebut ke segmen lain terlebih dahulu.", subject, strings.Join(parts, ", "))
}

// ─── Tim ─────────────────────────────────────────────────────────────────────

func (h *Handler) GetMasterTeams(c *gin.Context) {
	var teams []models.MasterTeam
	h.DB.Preload("Branches", func(db *gorm.DB) *gorm.DB {
		return db.Order("branch_name ASC")
	}).Order("team_name ASC").Find(&teams)
	c.JSON(http.StatusOK, gin.H{"teams": teams})
}

type teamRequest struct {
	TeamName string `json:"team_name" binding:"required"`
}

func (h *Handler) CreateMasterTeam(c *gin.Context) {
	actor, ok := requireAdmin(c, "Hanya Admin yang dapat menambah Tim")
	if !ok {
		return
	}
	var req teamRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.TeamName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nama Tim wajib diisi"})
		return
	}
	name := strings.TrimSpace(req.TeamName)
	code := makeCode(name)

	var dup int64
	h.DB.Model(&models.MasterTeam{}).Where("team_code = ? OR LOWER(team_name) = LOWER(?)", code, name).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Tim '%s' sudah terdaftar", name)})
		return
	}

	team := models.MasterTeam{ID: uuid.NewString(), TeamCode: code, TeamName: name}
	if err := h.DB.Create(&team).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menambah Tim"})
		return
	}
	h.logMasterAction(actor, "ADD_TEAM", fmt.Sprintf("%s (%s) menambahkan Tim '%s'", actor.FullName, actor.BadgeNumber, team.TeamName))
	c.JSON(http.StatusCreated, gin.H{"team": team})
}

func (h *Handler) UpdateMasterTeam(c *gin.Context) {
	actor, ok := requireAdmin(c, "Hanya Admin yang dapat mengubah Tim")
	if !ok {
		return
	}
	var team models.MasterTeam
	if err := h.DB.First(&team, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tim tidak ditemukan"})
		return
	}
	var req teamRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.TeamName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nama Tim wajib diisi"})
		return
	}
	name := strings.TrimSpace(req.TeamName)

	var dup int64
	h.DB.Model(&models.MasterTeam{}).Where("id <> ? AND LOWER(team_name) = LOWER(?)", team.ID, name).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Tim '%s' sudah terdaftar", name)})
		return
	}

	oldName := team.TeamName
	if err := h.DB.Model(&team).Update("team_name", name).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengubah Tim"})
		return
	}
	h.logMasterAction(actor, "UPDATE_TEAM", fmt.Sprintf("%s (%s) mengubah nama Tim '%s' menjadi '%s'", actor.FullName, actor.BadgeNumber, oldName, name))
	c.JSON(http.StatusOK, gin.H{"team": team})
}

func (h *Handler) DeleteMasterTeam(c *gin.Context) {
	actor, ok := requireAdmin(c, "Hanya Admin yang dapat menghapus Tim")
	if !ok {
		return
	}
	var team models.MasterTeam
	if err := h.DB.First(&team, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tim tidak ditemukan"})
		return
	}
	// Pengguna/folder/dokumen ber-branch selalu juga menyimpan team_id, jadi cek level tim sudah mencakup branch-nya.
	users, folders, docs := h.countUsage("team_id", "assigned_team_id", "owner_team_id", team.ID)
	if users+folders+docs > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": usageMessage(fmt.Sprintf("Tim '%s'", team.TeamName), users, folders, docs)})
		return
	}

	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("team_id = ?", team.ID).Delete(&models.MasterBranch{}).Error; err != nil {
			return err
		}
		return tx.Delete(&team).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus Tim"})
		return
	}
	h.logMasterAction(actor, "DELETE_TEAM", fmt.Sprintf("%s (%s) menghapus Tim '%s' beserta branch-nya", actor.FullName, actor.BadgeNumber, team.TeamName))
	c.JSON(http.StatusOK, gin.H{"message": "Tim berhasil dihapus"})
}

// ─── Branch ──────────────────────────────────────────────────────────────────

type createBranchReq struct {
	TeamID     string `json:"team_id" binding:"required"`
	BranchName string `json:"branch_name" binding:"required"`
}

type updateBranchReq struct {
	BranchName string `json:"branch_name" binding:"required"`
}

func (h *Handler) branchNameTaken(teamID, name, excludeID string) bool {
	var dup int64
	h.DB.Model(&models.MasterBranch{}).
		Where("team_id = ? AND id <> ? AND LOWER(branch_name) = LOWER(?)", teamID, excludeID, name).
		Count(&dup)
	return dup > 0
}

func (h *Handler) CreateMasterBranch(c *gin.Context) {
	actor, ok := requireAdmin(c, "Hanya Admin yang dapat menambah Branch Tim")
	if !ok {
		return
	}
	var req createBranchReq
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.BranchName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Data branch tidak lengkap"})
		return
	}
	var team models.MasterTeam
	if err := h.DB.First(&team, "id = ?", req.TeamID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tim tidak ditemukan"})
		return
	}
	name := strings.TrimSpace(req.BranchName)
	if h.branchNameTaken(team.ID, name, "") {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Branch '%s' sudah ada di %s", name, team.TeamName)})
		return
	}

	branch := models.MasterBranch{
		ID:         uuid.NewString(),
		TeamID:     team.ID,
		BranchCode: makeCode(name),
		BranchName: name,
	}
	if err := h.DB.Create(&branch).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Gagal menambah branch"})
		return
	}
	h.DB.Model(&models.MasterTeam{}).Where("id = ?", team.ID).Update("has_branches", true)
	h.logMasterAction(actor, "ADD_BRANCH", fmt.Sprintf("%s (%s) menambahkan Branch '%s' ke %s", actor.FullName, actor.BadgeNumber, name, team.TeamName))
	c.JSON(http.StatusCreated, gin.H{"branch": branch})
}

func (h *Handler) UpdateMasterBranch(c *gin.Context) {
	actor, ok := requireAdmin(c, "Hanya Admin yang dapat mengubah Branch")
	if !ok {
		return
	}
	var branch models.MasterBranch
	if err := h.DB.First(&branch, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Branch tidak ditemukan"})
		return
	}
	var req updateBranchReq
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.BranchName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nama Branch wajib diisi"})
		return
	}
	name := strings.TrimSpace(req.BranchName)
	if h.branchNameTaken(branch.TeamID, name, branch.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Branch '%s' sudah ada di tim ini", name)})
		return
	}

	oldName := branch.BranchName
	if err := h.DB.Model(&branch).Update("branch_name", name).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengubah Branch"})
		return
	}
	h.logMasterAction(actor, "UPDATE_BRANCH", fmt.Sprintf("%s (%s) mengubah nama Branch '%s' menjadi '%s'", actor.FullName, actor.BadgeNumber, oldName, name))
	c.JSON(http.StatusOK, gin.H{"branch": branch})
}

func (h *Handler) DeleteMasterBranch(c *gin.Context) {
	actor, ok := requireAdmin(c, "Hanya Admin yang dapat menghapus Branch")
	if !ok {
		return
	}
	var branch models.MasterBranch
	if err := h.DB.First(&branch, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Branch tidak ditemukan"})
		return
	}
	users, folders, docs := h.countUsage("branch_id", "assigned_branch_id", "owner_branch_id", branch.ID)
	if users+folders+docs > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": usageMessage(fmt.Sprintf("Branch '%s'", branch.BranchName), users, folders, docs)})
		return
	}

	if err := h.DB.Delete(&branch).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus Branch"})
		return
	}
	var remaining int64
	h.DB.Model(&models.MasterBranch{}).Where("team_id = ?", branch.TeamID).Count(&remaining)
	if remaining == 0 {
		h.DB.Model(&models.MasterTeam{}).Where("id = ?", branch.TeamID).Update("has_branches", false)
	}
	h.logMasterAction(actor, "DELETE_BRANCH", fmt.Sprintf("%s (%s) menghapus Branch '%s'", actor.FullName, actor.BadgeNumber, branch.BranchName))
	c.JSON(http.StatusOK, gin.H{"message": "Branch berhasil dihapus"})
}

// ─── Master Akses (Users) ────────────────────────────────────────────────────

func (h *Handler) GetMasterUsers(c *gin.Context) {
	if _, ok := requireAdmin(c, "Hanya Admin yang dapat melihat Master Akses"); !ok {
		return
	}
	var users []models.User
	h.DB.Preload("Team").Preload("Branch").Order("created_at ASC").Find(&users)
	c.JSON(http.StatusOK, gin.H{"users": users})
}

type createUserRequest struct {
	BadgeNumber string  `json:"badge_number" binding:"required"`
	FullName    string  `json:"full_name" binding:"required"`
	Email       string  `json:"email" binding:"required"`
	Role        string  `json:"role" binding:"required"` // SUPER_ADMIN | ADMIN | PEGAWAI_TI
	TeamID      *string `json:"team_id"`
	BranchID    *string `json:"branch_id"`
}

type updateUserRequest struct {
	FullName string  `json:"full_name" binding:"required"`
	Email    string  `json:"email" binding:"required"`
	Role     string  `json:"role" binding:"required"`
	TeamID   *string `json:"team_id"`
	BranchID *string `json:"branch_id"`
	IsActive *bool   `json:"is_active"`
}

// validateRoleAssignment: role SUPER_ADMIN hanya dapat diberikan oleh Super Admin.
func validateRoleAssignment(actor models.User, role string) (int, string) {
	if !models.IsValidRole(role) {
		return http.StatusBadRequest, "Role tidak dikenal"
	}
	if role == models.RoleSuperAdmin && actor.Role != models.RoleSuperAdmin {
		return http.StatusForbidden, "Hanya Super Admin yang dapat memberikan role SUPER_ADMIN"
	}
	return 0, ""
}

func (h *Handler) CreateMasterUser(c *gin.Context) {
	admin, ok := requireAdmin(c, "Hanya Admin yang dapat mengelola Master Akses")
	if !ok {
		return
	}

	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Badge, Nama, Email, dan Role wajib diisi"})
		return
	}
	if status, msg := validateRoleAssignment(admin, req.Role); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	badge := strings.TrimSpace(req.BadgeNumber)
	var existing models.User
	if err := h.DB.Where("badge_number = ?", badge).First(&existing).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Badge %s sudah terdaftar", badge)})
		return
	}

	teamID, branchID, segErr := h.resolveSegment(req.Role, req.TeamID, req.BranchID)
	if segErr != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": segErr})
		return
	}

	newUser := models.User{
		ID:           uuid.NewString(),
		BadgeNumber:  badge,
		FullName:     strings.TrimSpace(req.FullName),
		Email:        strings.TrimSpace(req.Email),
		PasswordHash: "Panda",
		Role:         req.Role,
		TeamID:       teamID,
		BranchID:     branchID,
		IsActive:     true,
	}

	if err := h.DB.Create(&newUser).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan akses pengguna"})
		return
	}

	h.DB.Preload("Team").Preload("Branch").First(&newUser, "id = ?", newUser.ID)
	h.logMasterAction(admin, "ADD_USER_ACCESS", fmt.Sprintf("Admin %s (%s) mendaftarkan akses Badge %s (%s) sebagai %s - %s",
		admin.FullName, admin.BadgeNumber, newUser.BadgeNumber, newUser.FullName, newUser.Role, formatUserTeamLabel(newUser)))

	c.JSON(http.StatusCreated, gin.H{"user": newUser})
}

func (h *Handler) UpdateMasterUser(c *gin.Context) {
	admin, ok := requireAdmin(c, "Hanya Admin yang dapat mengelola Master Akses")
	if !ok {
		return
	}
	var target models.User
	if err := h.DB.Preload("Team").Preload("Branch").First(&target, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User tidak ditemukan"})
		return
	}

	var req updateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nama, Email, dan Role wajib diisi"})
		return
	}
	if target.Role == models.RoleSuperAdmin && admin.Role != models.RoleSuperAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "Akun Super Admin hanya dapat diubah oleh Super Admin"})
		return
	}
	if status, msg := validateRoleAssignment(admin, req.Role); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	if target.BadgeNumber == initialAdminBadge() && req.Role != models.RoleSuperAdmin {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Badge awal %s harus tetap Super Admin", target.BadgeNumber)})
		return
	}
	if target.ID == admin.ID && req.Role == models.RolePegawaiTI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Anda tidak dapat menurunkan role akun Anda sendiri"})
		return
	}
	if req.IsActive != nil && !*req.IsActive && (target.ID == admin.ID || target.BadgeNumber == initialAdminBadge()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Akun ini tidak dapat dinonaktifkan"})
		return
	}

	teamID, branchID, segErr := h.resolveSegment(req.Role, req.TeamID, req.BranchID)
	if segErr != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": segErr})
		return
	}

	before := fmt.Sprintf("%s - %s", target.Role, formatUserTeamLabel(target))
	updates := map[string]interface{}{
		"full_name": strings.TrimSpace(req.FullName),
		"email":     strings.TrimSpace(req.Email),
		"role":      req.Role,
		"team_id":   nullable(teamID),
		"branch_id": nullable(branchID),
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}
	if err := h.DB.Model(&models.User{}).Where("id = ?", target.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan perubahan akses"})
		return
	}

	var updated models.User
	h.DB.Preload("Team").Preload("Branch").First(&updated, "id = ?", target.ID)
	after := fmt.Sprintf("%s - %s", updated.Role, formatUserTeamLabel(updated))
	h.logMasterAction(admin, "UPDATE_USER_ACCESS", fmt.Sprintf("%s (%s) mengubah akses Badge %s (%s): %s → %s",
		admin.FullName, admin.BadgeNumber, updated.BadgeNumber, updated.FullName, before, after))

	c.JSON(http.StatusOK, gin.H{"user": updated})
}

func (h *Handler) DeleteMasterUser(c *gin.Context) {
	admin, ok := requireAdmin(c, "Hanya Admin yang dapat menghapus akses")
	if !ok {
		return
	}
	var target models.User
	if err := h.DB.First(&target, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User tidak ditemukan"})
		return
	}
	switch {
	case target.BadgeNumber == initialAdminBadge():
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Super Admin utama (Badge %s) tidak dapat dihapus", target.BadgeNumber)})
		return
	case target.ID == admin.ID:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Anda tidak dapat menghapus akun Anda sendiri"})
		return
	case target.Role == models.RoleSuperAdmin && admin.Role != models.RoleSuperAdmin:
		c.JSON(http.StatusForbidden, gin.H{"error": "Akun Super Admin hanya dapat dihapus oleh Super Admin"})
		return
	}
	if err := h.DB.Delete(&target).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus akses pengguna"})
		return
	}
	h.logMasterAction(admin, "DELETE_USER_ACCESS", fmt.Sprintf("%s (%s) menghapus akses Badge %s (%s)", admin.FullName, admin.BadgeNumber, target.BadgeNumber, target.FullName))
	c.JSON(http.StatusOK, gin.H{"message": "Akses pengguna berhasil dihapus"})
}
