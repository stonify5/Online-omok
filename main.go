package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// Constants
const serverAddress = ":8080"

const (
	BoardSize         = 15
	TotalCells        = BoardSize * BoardSize
	MaxNicknameLength = 10
	TimeoutDuration   = 60 * time.Second
	WinningCount      = 5 // For Omok, 5 stones in a row wins.
)

const (
	black   uint8 = 1
	white   uint8 = 2
	emptied uint8 = 0
)

// Game status codes sent to clients
const (
	StatusWin          = 0 // You won
	StatusLoss         = 1 // You lost
	StatusUser2Timeout = 2 // Opponent timed out
	StatusUser1Timeout = 3 // Opponent timed out (from user2's perspective)
	StatusErrorReading = 4 // Error reading opponent's move
)

const (
	WebSocketPingType = "ping"
	WebSocketPongType = "pong"
)

// Types
type OmokRoom struct {
	board_15x15   [TotalCells]uint8
	user1         user
	user2         user
	spectators    []*websocket.Conn
	spectatorsMux sync.Mutex // Protects 'spectators' list for this room
}

type user struct {
	ws       *websocket.Conn
	check    bool // True if this user slot is active
	nickname string
}

// Message structure for WebSocket communication
type Message struct {
	Data      interface{} `json:"data,omitempty"`
	YourColor interface{} `json:"YourColor,omitempty"`
	Message   interface{} `json:"message,omitempty"` // Can be game status, error, or text
	NumUsers  interface{} `json:"numUsers,omitempty"`
	Nickname  interface{} `json:"nickname,omitempty"` // Opponent's nickname
}

type SpectatorMessage struct {
	Board interface{} `json:"board,omitempty"`
	Data  interface{} `json:"data,omitempty"`  // Move data
	Color interface{} `json:"color,omitempty"` // Color of the stone placed
	User1 interface{} `json:"user1,omitempty"` // Nickname of user1 (black)
	User2 interface{} `json:"user2,omitempty"` // Nickname of user2 (white)
}

// Global variables
var (
	upgrader = websocket.Upgrader{
		// Allow cross-origin requests for development and deployment
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	rooms            []*OmokRoom
	sockets          []*websocket.Conn // List of all active WebSocket connections (players + spectators)
	globalMutex      sync.Mutex        // Protects 'rooms', 'sockets', and 'connectionsCount'
	connectionsCount = 0
)

// Handles matching users to rooms or creating new rooms.
func RoomMatching(ws *websocket.Conn) {
	globalMutex.Lock()
	connectionsCount++
	currentConnections := connectionsCount
	globalMutex.Unlock()
	BroadcastConnectionsCount(currentConnections) // Broadcast with the new count

	log.Printf("Connection attempt from %s. Waiting for nickname.", ws.RemoteAddr())
	_, nicknameBytes, err := ws.ReadMessage()
	nickname := string(nicknameBytes)
	nicknameLength := utf8.RuneCountInString(nickname)

	if err != nil {
		log.Printf("Error reading nickname from %s: %v", ws.RemoteAddr(), err)
		handleConnectionFailure(ws)
		return
	}
	if nicknameLength == 0 {
		log.Printf("Empty nickname received from %s", ws.RemoteAddr())
		handleConnectionFailure(ws)
		return
	}
	if nicknameLength > MaxNicknameLength {
		log.Printf("Nickname too long from %s: %d characters (max %d)", ws.RemoteAddr(), nicknameLength, MaxNicknameLength)
		handleConnectionFailure(ws)
		return
	}

	log.Printf("Nickname received from %s: %s", ws.RemoteAddr(), nickname)

	globalMutex.Lock()
	defer globalMutex.Unlock()

	// Clean up disconnected waiting players before matching
	cleanupDisconnectedWaitingPlayers()

	for _, room := range rooms {
		if room.user1.check && !room.user2.check { // Found a room with one player waiting
			// Quick check - if WebSocket is nil, skip this room and let cleanup handle it
			if room.user1.ws != nil {
				// Just assign user2 to this room - don't ping during matching
				room.user2.check = true
				room.user2.ws = ws
				room.user2.nickname = nickname
				log.Printf("User %s (%s) joined User %s (%s) in a room.", nickname, ws.RemoteAddr(), room.user1.nickname, room.user1.ws.RemoteAddr())

				// Unlock globalMutex before starting MessageHandler as it's a long-running blocking call
				// and MessageHandler itself will manage room state.
				go room.MessageHandler()
				return // Successfully matched
			} else {
				// Waiting player WebSocket is nil, this room will be cleaned up
				log.Printf("Skipping room with nil WebSocket for waiting player: %s", room.user1.nickname)
			}
		}
	}

	// No suitable room found, create a new one for this player as user1
	newRoom := &OmokRoom{}
	newRoom.user1.check = true
	newRoom.user1.ws = ws
	newRoom.user1.nickname = nickname
	rooms = append(rooms, newRoom)
	log.Printf("User %s (%s) created a new room.", nickname, ws.RemoteAddr())
	// User1 will wait in this room; MessageHandler is not started until user2 joins.
}

// Main game loop for a room.
func (room *OmokRoom) MessageHandler() {
	log.Printf("Game starting between %s (black) and %s (white).", room.user1.nickname, room.user2.nickname)

	// Notify players of game start and their colors/opponent's nickname
	err1 := room.user1.ws.WriteJSON(Message{YourColor: "black", Nickname: room.user2.nickname})
	err2 := room.user2.ws.WriteJSON(Message{YourColor: "white", Nickname: room.user1.nickname})

	if err1 != nil || err2 != nil {
		log.Printf("Error sending initial game messages to %s or %s. Resetting room.", room.user1.nickname, room.user2.nickname)
		room.reset()
		return
	}

	var currentMoveIndex int
	var timeout bool
	var readErr bool

	for {
		// Black's turn (user1)
		currentMoveIndex, timeout, readErr = reading(room.user1.ws)
		if timeout {
			log.Printf("User %s (black) timed out. %s (white) wins.", room.user1.nickname, room.user2.nickname)
			if room.user2.ws != nil {
				room.user2.ws.WriteJSON(Message{Message: StatusUser1Timeout})
			}
			if room.user1.ws != nil {
				room.user1.ws.WriteJSON(Message{Message: StatusLoss})
			}
			room.reset()
			return
		}
		if readErr {
			log.Printf("Error reading from %s (black). %s (white) wins by default.", room.user1.nickname, room.user2.nickname)
			if room.user2.ws != nil {
				room.user2.ws.WriteJSON(Message{Message: StatusErrorReading})
			}
			room.reset()
			return
		}
		if room.isValidMove(currentMoveIndex) {
			room.board_15x15[currentMoveIndex] = black
			if room.user2.ws != nil {
				if err := room.user2.ws.WriteJSON(Message{Data: currentMoveIndex}); err != nil {
					log.Printf("Error sending move to %s (white). Resetting room.", room.user2.nickname)
					room.reset()
					return
				}
			}
			room.broadcastMoveToSpectators(currentMoveIndex, black)
			if room.VictoryConfirm(currentMoveIndex) {
				log.Printf("User %s (black) wins!", room.user1.nickname)
				if room.user1.ws != nil {
					room.user1.ws.WriteJSON(Message{Message: StatusWin})
				}
				if room.user2.ws != nil {
					room.user2.ws.WriteJSON(Message{Message: StatusLoss})
				}
				room.reset()
				return
			}
		} else {
			log.Printf("Invalid move from %s (black). Game reset.", room.user1.nickname)
			room.reset()
			return
		}

		// White's turn (user2)
		currentMoveIndex, timeout, readErr = reading(room.user2.ws)
		if timeout {
			log.Printf("User %s (white) timed out. %s (black) wins.", room.user2.nickname, room.user1.nickname)
			if room.user1.ws != nil {
				room.user1.ws.WriteJSON(Message{Message: StatusUser2Timeout})
			}
			if room.user2.ws != nil {
				room.user2.ws.WriteJSON(Message{Message: StatusLoss})
			}
			room.reset()
			return
		}
		if readErr {
			log.Printf("Error reading from %s (white). %s (black) wins by default.", room.user2.nickname, room.user1.nickname)
			if room.user1.ws != nil {
				room.user1.ws.WriteJSON(Message{Message: StatusErrorReading})
			}
			room.reset()
			return
		}
		if room.isValidMove(currentMoveIndex) {
			room.board_15x15[currentMoveIndex] = white
			if room.user1.ws != nil {
				if err := room.user1.ws.WriteJSON(Message{Data: currentMoveIndex}); err != nil {
					log.Printf("Error sending move to %s (black). Resetting room.", room.user1.nickname)
					room.reset()
					return
				}
			}
			room.broadcastMoveToSpectators(currentMoveIndex, white)
			if room.VictoryConfirm(currentMoveIndex) {
				log.Printf("User %s (white) wins!", room.user2.nickname)
				if room.user2.ws != nil {
					room.user2.ws.WriteJSON(Message{Message: StatusWin})
				}
				if room.user1.ws != nil {
					room.user1.ws.WriteJSON(Message{Message: StatusLoss})
				}
				room.reset()
				return
			}
		} else {
			log.Printf("Invalid move from %s (white). Game reset.", room.user2.nickname)
			room.reset()
			return
		}
	}
}

func (room *OmokRoom) isValidMove(index int) bool {
	return index >= 0 && index < TotalCells && room.board_15x15[index] == emptied
}

func (room *OmokRoom) broadcastMoveToSpectators(moveIndex int, color uint8) {
	room.spectatorsMux.Lock()
	defer room.spectatorsMux.Unlock()

	message := SpectatorMessage{Data: moveIndex, Color: color}
	for _, spectatorWs := range room.spectators {
		if spectatorWs != nil {
			if err := spectatorWs.WriteJSON(message); err != nil {
				log.Printf("Error broadcasting move to spectator %s: %v. Will be cleaned up.", spectatorWs.RemoteAddr(), err)
				// Mark spectator for removal or handle error; removal is usually done by spectator's own handler.
			}
		}
	}
}

// VictoryConfirm checks if the last move at 'index' resulted in a win.
// Assumes 'index' is a valid move and the stone is already placed on room.board_15x15.
func (room *OmokRoom) VictoryConfirm(index int) bool {
	stoneColor := room.board_15x15[index]
	if stoneColor == emptied {
		return false // Should not happen if called after a move
	}

	// Directions to check: horizontal, vertical, diagonal (down-right), diagonal (down-left)
	// dx, dy pairs for these directions
	checkDirections := [][2]int{
		{0, 1},  // Horizontal (col changes)
		{1, 0},  // Vertical (row changes)
		{1, 1},  // Diagonal \ (row and col change same way)
		{1, -1}, // Diagonal / (row and col change opposite ways)
	}

	y := index / BoardSize
	x := index % BoardSize

	for _, dir := range checkDirections {
		dy, dx := dir[0], dir[1]
		count := 1 // Count the stone just placed

		// Check in the positive direction (e.g., right, down, down-right, down-left)
		for i := 1; i < WinningCount; i++ {
			curX, curY := x+dx*i, y+dy*i
			if curX >= 0 && curX < BoardSize && curY >= 0 && curY < BoardSize &&
				room.board_15x15[curY*BoardSize+curX] == stoneColor {
				count++
			} else {
				break
			}
		}

		// Check in the negative direction (e.g., left, up, up-left, up-right)
		for i := 1; i < WinningCount; i++ {
			curX, curY := x-dx*i, y-dy*i
			if curX >= 0 && curX < BoardSize && curY >= 0 && curY < BoardSize &&
				room.board_15x15[curY*BoardSize+curX] == stoneColor {
				count++
			} else {
				break
			}
		}
		// "장목 불가" (no overlines win) rule means exactly WinningCount stones.
		if count == WinningCount {
			return true
		}
	}
	return false
}

func (room *OmokRoom) SendVictoryMessage(winnerColor uint8) {
	// This function seems to be duplicated by logic within MessageHandler.
	// Keeping it for now if it's intended for other uses, but MessageHandler directly sends win/loss.
	// If winnerColor is black (user1)
	if winnerColor == black {
		if room.user1.ws != nil {
			room.user1.ws.WriteJSON(Message{Message: StatusWin})
		}
		if room.user2.ws != nil {
			room.user2.ws.WriteJSON(Message{Message: StatusLoss})
		}
	} else { // winnerColor is white (user2)
		if room.user2.ws != nil {
			room.user2.ws.WriteJSON(Message{Message: StatusWin})
		}
		if room.user1.ws != nil {
			room.user1.ws.WriteJSON(Message{Message: StatusLoss})
		}
	}
}

// Reads a message from WebSocket with timeout.
// Returns (message_as_int, timeout_occurred, other_error_occurred)
func reading(ws *websocket.Conn) (int, bool, bool) {
	if ws == nil {
		return 0, false, true // Error if ws is nil
	}

	for {
		ws.SetReadDeadline(time.Now().Add(TimeoutDuration))
		_, msgBytes, err := ws.ReadMessage()
		ws.SetReadDeadline(time.Time{}) // Clear the deadline

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return 0, true, false // Timeout
			}
			log.Printf("Error reading from WebSocket %s: %v", ws.RemoteAddr(), err)
			return 0, false, true // Other error
		}

		msgStr := string(msgBytes)

		// Handle ping/pong messages - don't treat them as game moves
		if msgStr == WebSocketPongType {
			// Client responded to our ping, continue reading for actual game move
			continue
		}

		// Check if this is a JSON ping message
		var jsonMsg map[string]interface{}
		if json.Unmarshal(msgBytes, &jsonMsg) == nil {
			if msgType, exists := jsonMsg["type"]; exists && msgType == WebSocketPingType {
				// Send pong response
				ws.WriteJSON(map[string]interface{}{"type": WebSocketPongType})
				continue
			}
		}

		// Try to parse as game move
		moveIndex, convErr := strconv.Atoi(msgStr)
		if convErr != nil {
			log.Printf("Error converting message to int from %s ('%s'): %v", ws.RemoteAddr(), msgStr, convErr)
			return 0, false, true // Conversion error
		}
		return moveIndex, false, false
	}
}

// Prepares a room for reset by nullifying its user WebSockets.
// Actual removal from global lists and count decrements should be handled carefully with locks.
func (room *OmokRoom) prepareForReset() {
	if room.user1.ws != nil {
		room.user1.ws.Close() // Attempt to close, ignore error as we are resetting anyway
		// Decrementing connectionsCount is done by the caller or a central cleanup
	}
	if room.user2.ws != nil {
		room.user2.ws.Close()
	}
	room.user1.ws = nil
	room.user2.ws = nil
	room.user1.check = false
	room.user2.check = false

	// Clear spectators for this room
	room.spectatorsMux.Lock()
	for _, specWs := range room.spectators {
		if specWs != nil {
			specWs.Close() // Close spectator connections for this room
			// Spectator's own handler (handleSocketError) should manage global 'sockets' and 'connectionsCount'
		}
	}
	room.spectators = nil // Clear the list
	room.spectatorsMux.Unlock()
}

// Resets a game room, closes connections, and cleans up.
func (room *OmokRoom) reset() {
	log.Printf("Resetting room (User1: %s, User2: %s)...", room.user1.nickname, room.user2.nickname)

	// Store WebSocket references before nullifying them
	user1WS := room.user1.ws
	user2WS := room.user2.ws

	room.prepareForReset() // Close WebSockets and clear user checks

	globalMutex.Lock()
	defer globalMutex.Unlock()

	// Properly handle connection count decrements for players
	// Note: prepareForReset already closed the WebSockets, but we need to manage the count
	connectionsDecremented := 0

	if user1WS != nil {
		removeWebSocketFromSocketsUnsafe(user1WS)
		connectionsCount--
		connectionsDecremented++
		log.Printf("Decremented connection count for user1 %s", room.user1.nickname)
	}
	if user2WS != nil {
		removeWebSocketFromSocketsUnsafe(user2WS)
		connectionsCount--
		connectionsDecremented++
		log.Printf("Decremented connection count for user2 %s", room.user2.nickname)
	}

	removeRoomFromRoomsUnsafe(room)
	currentConnections := connectionsCount

	if connectionsDecremented > 0 {
		log.Printf("Room reset: decremented %d connections, current total: %d", connectionsDecremented, currentConnections)
	}

	BroadcastConnectionsCountUnsafe(currentConnections)
}

// General handler for a failed/closed WebSocket connection.
func handleConnectionFailure(ws *websocket.Conn) {
	if ws == nil {
		return
	}
	log.Printf("Handling connection failure/closure for %s", ws.RemoteAddr())
	ws.Close()

	globalMutex.Lock()
	defer globalMutex.Unlock()

	// Check if WebSocket is still in the sockets list before removing and decrementing
	wasInList := false
	for _, socket := range sockets {
		if socket == ws {
			wasInList = true
			break
		}
	}

	if wasInList {
		removeWebSocketFromSocketsUnsafe(ws)
		connectionsCount--
		currentConnections := connectionsCount
		log.Printf("Connection count decremented for %s, current total: %d", ws.RemoteAddr(), currentConnections)
		BroadcastConnectionsCountUnsafe(currentConnections)
	} else {
		log.Printf("WebSocket %s already removed from connections list", ws.RemoteAddr())
	}
}

// Removes a room from the global 'rooms' list. Assumes globalMutex is held by caller.
func removeRoomFromRoomsUnsafe(roomToRemove *OmokRoom) {
	newRooms := []*OmokRoom{}
	for _, r := range rooms {
		if r != roomToRemove {
			newRooms = append(newRooms, r)
		}
	}
	rooms = newRooms
	log.Printf("Room removed. Current rooms: %d", len(rooms))
}

// Broadcasts the current number of connections to all sockets. Assumes globalMutex is held by caller.
func BroadcastConnectionsCountUnsafe(count int) {
	log.Printf("Broadcasting connection count: %d to %d sockets", count, len(sockets))
	message := Message{NumUsers: count}
	for _, s := range sockets {
		if s != nil {
			if err := s.WriteJSON(message); err != nil {
				// Log error, but don't let one bad socket stop broadcast to others.
				// Bad sockets will be cleaned up by their own handlers.
				log.Printf("Error broadcasting connection count to %s: %v", s.RemoteAddr(), err)
			}
		}
	}
}

// Wrapper for broadcasting connection count that handles locking.
func BroadcastConnectionsCount(count int) {
	globalMutex.Lock()
	defer globalMutex.Unlock()
	BroadcastConnectionsCountUnsafe(count)
}

// Upgrades HTTP connection to WebSocket and adds to global list.
func upgradeWebSocketConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed for %s: %v", r.RemoteAddr, err) // Corrected: r.RemoteAddr without parentheses
		return nil, err
	}
	log.Printf("WebSocket connection established from %s", ws.RemoteAddr())

	globalMutex.Lock()
	sockets = append(sockets, ws)
	globalMutex.Unlock()
	// Note: connectionsCount is incremented by player/spectator specific logic, not here.
	return ws, nil
}

// HTTP handler for game connections.
func SocketHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgradeWebSocketConnection(w, r)
	if err != nil {
		return // Error already logged by upgradeWebSocketConnection
	}

	// Always ensure cleanup happens when this handler exits
	defer handleConnectionFailure(ws)

	RoomMatching(ws)
}

// HTTP handler for spectator connections.
func SpectatorHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgradeWebSocketConnection(w, r)
	if err != nil {
		return
	}
	defer handleConnectionFailure(ws) // Ensures cleanup when spectator goroutine exits or handler returns.

	globalMutex.Lock()
	connectionsCount++
	currentConnections := connectionsCount
	globalMutex.Unlock()
	BroadcastConnectionsCount(currentConnections)

	go func(spectatorWs *websocket.Conn) {
		log.Printf("Spectator %s connected. Finding a room.", spectatorWs.RemoteAddr())
		var assignedRoom *OmokRoom
		var lastSentBoardState [TotalCells]uint8 // To avoid sending redundant board states
		connectionCheckCounter := 0              // Only check connection periodically

		for {
			// Only check connection every 10 iterations to reduce load
			connectionCheckCounter++
			if connectionCheckCounter%10 == 0 && !IsWebSocketConnected(spectatorWs) {
				log.Printf("Spectator %s disconnected while waiting/watching.", spectatorWs.RemoteAddr())
				if assignedRoom != nil {
					removeSocketFromSpectators(assignedRoom, spectatorWs)
				}
				return // Exit goroutine, defer will call handleConnectionFailure
			}

			globalMutex.Lock()
			foundRoomThisCycle := false
			for _, room := range rooms {
				if room.user1.check && room.user2.check { // Active game
					if assignedRoom != room { // New room or first assignment
						if assignedRoom != nil { // Was watching another room
							removeSocketFromSpectators(assignedRoom, spectatorWs)
						}
						assignedRoom = room
						addSocketToSpectators(assignedRoom, spectatorWs)
						log.Printf("Spectator %s now watching game between %s and %s.", spectatorWs.RemoteAddr(), room.user1.nickname, room.user2.nickname)

						// Send full initial state for the new room
						assignedRoom.spectatorsMux.Lock()     // Lock before accessing room board
						boardCopy := assignedRoom.board_15x15 // Make a copy to send
						lastSentBoardState = boardCopy
						assignedRoom.spectatorsMux.Unlock()

						initialMsg := SpectatorMessage{
							Board: boardCopy,
							User1: room.user1.nickname,
							User2: room.user2.nickname,
						}
						if err := spectatorWs.WriteJSON(initialMsg); err != nil {
							log.Printf("Error sending initial state to spectator %s: %v", spectatorWs.RemoteAddr(), err)
							removeSocketFromSpectators(assignedRoom, spectatorWs) // Clean up from this room
							return                                                // Exit goroutine
						}
					} else { // Still watching the same room, check if board updated
						assignedRoom.spectatorsMux.Lock()
						boardChanged := false
						if assignedRoom.board_15x15 != lastSentBoardState {
							boardChanged = true
							lastSentBoardState = assignedRoom.board_15x15
						}
						boardCopy := lastSentBoardState // Use the latest state
						assignedRoom.spectatorsMux.Unlock()

						if boardChanged {
							updateMsg := SpectatorMessage{Board: boardCopy}
							if err := spectatorWs.WriteJSON(updateMsg); err != nil {
								log.Printf("Error sending board update to spectator %s: %v", spectatorWs.RemoteAddr(), err)
								removeSocketFromSpectators(assignedRoom, spectatorWs)
								return
							}
						}
					}
					foundRoomThisCycle = true
					break // Found a room to spectate
				}
			}
			globalMutex.Unlock()

			if !foundRoomThisCycle { // No active games
				if assignedRoom != nil { // Was watching a room that ended
					log.Printf("Game ended for spectator %s. Searching for new game.", spectatorWs.RemoteAddr())
					removeSocketFromSpectators(assignedRoom, spectatorWs)
					assignedRoom = nil
				}
				if err := spectatorWs.WriteJSON(Message{Message: "No active games to spectate. Waiting..."}); err != nil {
					log.Printf("Error sending waiting message to spectator %s: %v", spectatorWs.RemoteAddr(), err)
					return // Exit goroutine
				}
			}

			// Keep-alive or periodic check for client pongs if not relying on ReadMessage
			// For now, ReadMessage (even if expecting no data) can detect closure.
			// Or, rely on IsWebSocketConnected check at the start of the loop.
			// To make it more responsive to client closing, try a non-blocking read or short-timeout read.
			// The current IsWebSocketConnected sends a ping and expects a pong.
			// If we don't expect messages from spectator, ReadMessage will block.
			// The IsWebSocketConnected check at loop start is the main liveness check.
			time.Sleep(2 * time.Second) // Poll for new games or game state changes
		}
	}(ws)
}

// Checks if a WebSocket connection is still active by sending an application-level ping.
func IsWebSocketConnected(conn *websocket.Conn) bool {
	if conn == nil {
		return false
	}

	// Set a longer deadline for the write operation to avoid premature timeouts
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := conn.WriteJSON(map[string]interface{}{"type": WebSocketPingType})
	conn.SetWriteDeadline(time.Time{}) // Clear write deadline

	if err != nil {
		// Only log actual network errors, not normal timeouts
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			log.Printf("IsWebSocketConnected: Failed to send Ping to %s: %v", conn.RemoteAddr(), err)
		}
		return false
	}

	// Give more time for pong response to avoid false disconnections
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) // Expect pong within 10 seconds
	msgType, pongMsg, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{}) // Clear read deadline

	if err != nil {
		// Don't log timeouts as errors since they're expected for disconnected clients
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			log.Printf("IsWebSocketConnected: Failed to receive Pong from %s: %v", conn.RemoteAddr(), err)
		}
		return false
	}

	if msgType == websocket.TextMessage && string(pongMsg) == WebSocketPongType {
		return true
	}

	// Accept any valid message as a sign of life, not just strict "pong" responses
	// This makes the connection check more forgiving
	if msgType == websocket.TextMessage {
		return true
	}

	return false
}

func addSocketToSpectators(room *OmokRoom, ws *websocket.Conn) {
	if room == nil || ws == nil {
		return
	}
	room.spectatorsMux.Lock()
	defer room.spectatorsMux.Unlock()
	// Avoid duplicates
	for _, existingWs := range room.spectators {
		if existingWs == ws {
			return
		}
	}
	room.spectators = append(room.spectators, ws)
}

func removeSocketFromSpectators(room *OmokRoom, wsToRemove *websocket.Conn) {
	if room == nil || wsToRemove == nil {
		return
	}
	room.spectatorsMux.Lock()
	defer room.spectatorsMux.Unlock()

	newSpectators := []*websocket.Conn{}
	for _, ws := range room.spectators {
		if ws != wsToRemove {
			newSpectators = append(newSpectators, ws)
		}
	}
	room.spectators = newSpectators
}

// Removes a WebSocket from the global 'sockets' list. Assumes globalMutex is held by caller.
func removeWebSocketFromSocketsUnsafe(wsToRemove *websocket.Conn) {
	if wsToRemove == nil {
		return
	}
	newSockets := []*websocket.Conn{}
	for _, ws := range sockets {
		if ws != wsToRemove {
			newSockets = append(newSockets, ws)
		}
	}
	sockets = newSockets
	log.Printf("Socket %s removed. Current total sockets: %d", wsToRemove.RemoteAddr(), len(sockets))
}

// Health check handler
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// Removes rooms with disconnected waiting players. Assumes globalMutex is held by caller.
func cleanupDisconnectedWaitingPlayers() {
	newRooms := []*OmokRoom{}
	connectionsDecremented := 0

	for _, room := range rooms {
		keepRoom := true

		// Check if room has only user1 waiting and they're disconnected
		if room.user1.check && !room.user2.check {
			// Check for nil WebSocket or closed connection
			if room.user1.ws == nil {
				log.Printf("Removing room with nil WebSocket for waiting player: %s", room.user1.nickname)
				keepRoom = false
			} else {
				// Check if connection is actually closed by attempting a quick ping
				// Use a very short timeout to avoid blocking
				room.user1.ws.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
				err := room.user1.ws.WriteJSON(map[string]interface{}{"type": "ping"})
				room.user1.ws.SetWriteDeadline(time.Time{})

				if err != nil {
					log.Printf("Removing room with disconnected waiting player: %s (error: %v)", room.user1.nickname, err)
					// Close the connection and clean up
					room.user1.ws.Close()
					removeWebSocketFromSocketsUnsafe(room.user1.ws)
					connectionsCount--
					connectionsDecremented++
					keepRoom = false
				}
			}
		}

		if keepRoom {
			newRooms = append(newRooms, room)
		}
	}

	rooms = newRooms

	if connectionsDecremented > 0 {
		log.Printf("Cleanup: removed %d disconnected waiting players, current connections: %d", connectionsDecremented, connectionsCount)
		BroadcastConnectionsCountUnsafe(connectionsCount)
	}
}

// Background cleanup for disconnected waiting players
func backgroundCleanup() {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	for range ticker.C {
		globalMutex.Lock()
		initialRoomCount := len(rooms)
		initialConnectionCount := connectionsCount

		cleanupDisconnectedWaitingPlayers()

		finalRoomCount := len(rooms)
		finalConnectionCount := connectionsCount

		if initialRoomCount != finalRoomCount || initialConnectionCount != finalConnectionCount {
			log.Printf("Background cleanup: rooms %d->%d, connections %d->%d",
				initialRoomCount, finalRoomCount, initialConnectionCount, finalConnectionCount)
		}
		globalMutex.Unlock()
	}
}

// Main function
func main() {
	// Start background cleanup for disconnected waiting players
	go backgroundCleanup()

	http.HandleFunc("/health", HealthHandler)
	http.HandleFunc("/game", SocketHandler)
	http.HandleFunc("/spectator", SpectatorHandler)

	log.Println("Omok server starting on", serverAddress)
	if err := http.ListenAndServe(serverAddress, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
