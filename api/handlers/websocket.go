package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/acheong08/obsidian-sync/database"
	"github.com/acheong08/obsidian-sync/utilities"
	"github.com/acheong08/obsidian-sync/vault"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// ReadBufferSize:  1024,
	// WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func getMsg(ws *websocket.Conn) ([]byte, error) {
	msgType, msg, err := ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	if msgType != websocket.TextMessage {
		return nil, errors.New("message type must be text")
	}
	return msg, nil
}

func WsHandler(c *gin.Context) {
	// Upgrade protocol to websocket
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println(err.Error())
		c.JSON(500, gin.H{
			"message": "error upgrading to websocket",
		})
		return
	}
	defer ws.Close()

	// Recieve initialization message
	msg, err := getMsg(ws)
	if err != nil {
		// Send error message
		ws.WriteJSON(gin.H{"error": err.Error()})
		return
	}
	// Parse initialization message as JSON
	connectionInfo, _, connectedVault, err := initHandler(msg, c.MustGet("db").(*database.Database))
	if err != nil {
		ws.WriteJSON(gin.H{"error": err.Error()})
		return
	}
	// No errors: ok response
	ws.WriteJSON(gin.H{"res": "ok"})
	if connectionInfo.Initial {
		var version int
		// connectionInfo.Version is an interface. Check what type it is and convert to int
		switch connectionInfo.Version.(type) {
		case string:
			version, err = strconv.Atoi(connectionInfo.Version.(string))
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
		case int:
			version = connectionInfo.Version.(int)

		}
		if err != nil {
			ws.WriteJSON(gin.H{"error": err.Error()})
			return
		}
		if connectedVault.Version != version {
			vaultFiles, err := vault.GetVaultFiles(connectedVault.ID)
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			for _, file := range *vaultFiles {
				if !file.Deleted {
					ws.WriteJSON(gin.H{
						"op": "push", "path": file.Path,
						"hash": file.Hash, "size": file.Size,
						"ctime": file.Created, "mtime": file.Modified, "folder": file.Folder,
						"deleted": file.Deleted, "device": "insignificantv5", "uid": file.UID})
				}
			}
		}
	}
	ws.WriteJSON(gin.H{"op": "ready", "version": connectedVault.Version})

	dbConnection := c.MustGet("db").(*database.Database)
	dbConnection.BumpVaultVersion(connectedVault.ID)

	// Inifinite loop to handle messages
	type message struct {
		Op string `json:"op" binding:"required"` // Operation
	}
	for {
		msg, err := getMsg(ws)
		if err != nil {
			// Send error message
			ws.WriteJSON(gin.H{"error": err.Error()})
			return
		}
		var m message
		err = json.Unmarshal(msg, &m)
		if err != nil {
			ws.WriteJSON(gin.H{"error": err.Error()})
			return
		}
		switch m.Op {
		case "size":
			size, err := vault.GetVaultSize(connectedVault.ID)
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			ws.WriteJSON(gin.H{
				"res":   "ok",
				"size":  size,
				"limit": 10737418240,
			})
		case "pull":
			var pull pullRequest
			err = json.Unmarshal(msg, &pull)
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			var uid int
			switch pull.UID.(type) {
			case string:
				uid, _ = strconv.Atoi(pull.UID.(string))
			case int:
				uid = pull.UID.(int)
			}
			fileMetadata, fileData, err := pullHandler(uid)
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			ws.WriteJSON(gin.H{
				"hash": fileMetadata.Hash, "size": len(*fileData), "pieces": 1,
				// Pieces is the number of 1KB pieces the file is split into
				// "pieces": math.Ceil(float64(len(*fileData)) / 1024),
			})
			// Send file data in 1KB pieces
			// for i := 0; i < len(*fileData); i += 1024 {
			// 	ws.WriteMessage(websocket.BinaryMessage, (*fileData)[i:int(math.Min(float64(i+1024), float64(len(*fileData))))])
			// }
			ws.WriteMessage(websocket.BinaryMessage, *fileData)
		case "push":
			var metadata struct {
				UID          int    `json:"uid"`
				Op           string `json:"op" binding:"required"` // Operation
				Path         string `json:"path" binding:"required"`
				Extension    string `json:"extension" binding:"required"`
				Hash         string `json:"hash" binding:"required"`
				CreationTime int64  `json:"ctime" binding:"required"`
				ModifiedTime int64  `json:"mtime" binding:"required"`
				Folder       bool   `json:"folder" binding:"required"`
				Deleted      bool   `json:"deleted" binding:"required"`
				Size         int    `json:"size,omitempty" binding:"required"`
				Pieces       int    `json:"pieces,omitempty" binding:"required"`
			}
			err = json.Unmarshal(msg, &metadata)
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			vaultUID, err := vault.InsertVaultFile(connectedVault.ID, vault.FileMetadata{
				Path:      metadata.Path,
				Hash:      metadata.Hash,
				Extension: metadata.Extension,
				Size:      int64(metadata.Size),
				Created:   metadata.CreationTime,
				Modified:  metadata.ModifiedTime,
				Folder:    metadata.Folder,
				Deleted:   metadata.Deleted,
			})
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			var fullBinary []byte
			for i := 0; i < metadata.Pieces; i++ {
				ws.WriteJSON(gin.H{"res": "next"})
				// Read bytes
				msgType, msg, err := ws.ReadMessage()
				if err != nil {
					ws.WriteJSON(gin.H{"error": err.Error()})
					return
				}
				if msgType != websocket.BinaryMessage {
					ws.WriteJSON(gin.H{"error": "message type must be binary"})
					return
				}
				fullBinary = append(fullBinary, msg...)
			}
			err = vault.PushFile(metadata.Path, &fullBinary)
			if err != nil {
				ws.WriteJSON(gin.H{"error": err.Error()})
				return
			}
			metadata.UID = int(vaultUID)
			ws.WriteJSON(metadata)
			ws.WriteJSON(gin.H{"op": "ok"})
		case "pong":
			ws.WriteJSON(gin.H{"op": "pong"})
		}
	}

}

type pullRequest struct {
	UID any `json:"uid" binding:"required"`
}

func pullHandler(uid int) (*vault.FileMetadata, *[]byte, error) {
	metadata, err := vault.GetVaultFile(uid)
	if err != nil {
		return nil, nil, err
	}
	data, err := vault.GetFile(metadata.Path)
	if err != nil {
		return nil, nil, err
	}
	return metadata, data, nil

}

type initializationRequest struct {
	Op      string `json:"op" binding:"required"` // Operation
	Token   string `json:"token" binding:"required"`
	Id      string `json:"id" binding:"required"`      // Vault ID
	KeyHash string `json:"keyhash" binding:"required"` // Hash of password & salt
	Version any    `json:"version" binding:"required"`
	Initial bool   `json:"initial" binding:"required"`
	Device  string `json:"device" binding:"required"`
}

func initHandler(req []byte, dbConnection *database.Database) (*initializationRequest, string, *vault.Vault, error) {

	var initial initializationRequest
	err := json.Unmarshal(req, &initial)
	if err != nil {
		return nil, "", nil, err
	}

	// Validate token and key hash
	email, err := utilities.GetJwtEmail(initial.Token)
	if err != nil {
		return nil, "", nil, err
	}

	vault, err := dbConnection.GetVault(initial.Id, initial.KeyHash)
	if err != nil {
		return nil, "", nil, err
	}
	return &initial, email, vault, nil
}