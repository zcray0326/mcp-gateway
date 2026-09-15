// Package model provides data models for the MCP Gateway application.
package model

import (
	"github.com/zcray0326/mcp-gateway/pkg/types"
	"gorm.io/gorm"
)

// User represents an authenticated, human user in enterprise mode.
// A user can be an admin or a regular user.
// There are no users if mcp-gateway is running in development mode.
type User struct {
	gorm.Model

	Username    string         `json:"username" gorm:"unique; not null"`
	Role        types.UserRole `json:"role" gorm:"not null"`
	AccessToken string         `json:"access_token" gorm:"unique; not null"`
}
