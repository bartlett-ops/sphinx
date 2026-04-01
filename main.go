package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type user struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

var users = make(map[string]user)

func main() {
	router := gin.Default()
	router.GET("/users", getUsers)
	router.POST("/users", postUsers)

	router.Run("localhost:8080")
}

func addUser(u user) {
	users[u.Email] = u
}

// getAlbums responds with the list of all albums as JSON.
func getUsers(c *gin.Context) {
	c.IndentedJSON(http.StatusOK, users)
}

// postAlbums adds an album from JSON received in the request body.
func postUsers(c *gin.Context) {
	requiredHeaders := []string{"X-User-Email", "X-Forwarded-For"}

	missing := []string{}

	for _, h := range requiredHeaders {
		if c.GetHeader(h) == "" {
			missing = append(missing, h)
		}
	}

	if len(missing) > 0 {
		c.JSON(400, gin.H{
			"error":   "Missing required headers",
			"missing": missing,
		})
		return
	}

	user := user{
		Email: c.Request.Header.Get("X-User-Email"),
		IP:    c.Request.Header.Get("X-Forwarded-For"),
	}

	addUser(user)

	c.IndentedJSON(http.StatusCreated, user)
}
