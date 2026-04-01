package main

import (
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type user struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

var users = make(map[string]user)

func main() {
	port := flag.Int("port", 8080, "Port to run server on")
	trustedProxiesRaw := flag.String("trusted-proxies", "", "Comma separated list of trusted proxies in CIDR format")
	flag.Parse()

	var trustedProxies []string

	if *trustedProxiesRaw != "" {
		trustedProxies = strings.Split(*trustedProxiesRaw, ",")
	}

	println(fmt.Sprintf("length trusted proxies: %d", len(trustedProxies)))

	router := gin.Default()
	router.SetTrustedProxies(trustedProxies)
	router.GET("/users", getUsers)
	router.POST("/users", postUsers)

	router.Run(fmt.Sprintf(":%d", *port))
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
	requiredHeaders := []string{"X-User-Email"}

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
		IP:    c.ClientIP(),
	}

	addUser(user)

	c.IndentedJSON(http.StatusCreated, user)
}
