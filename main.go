package main

import (
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
		// CheckOrigin can be used to validate the origin of the WebSocket request
		// For development, it's common to allow all origins:
		// CheckOrigin: func(r *http.Request) bool { return true },
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
	if err != nil || utf8.RuneCountInString(string(nicknameBytes)) > MaxNicknameLength {
		log.Printf("Error reading nickname or nickname too long from %s: %v", ws.RemoteAddr(), err)
		handleConnectionFailure(ws)
		return
	}
	nickname := string(nicknameBytes)
	log.Printf("Nickname received from %s: %s", ws.RemoteAddr(), nickname)

	globalMutex.Lock()
	defer globalMutex.Unlock()

	for _, room := range rooms {
		if room.user1.check && !room.user2.check { // Found a room with one player waiting
			if !IsWebSocketConnected(room.user1.ws) { // Check if user1 is still connected
				log.Printf("User1 in room (nickname: %s) disconnected. Resetting room.", room.user1.nickname)
				room.prepareForReset() // Mark for reset, actual reset happens after unlock or carefully
				// This room will be effectively reset or removed. Continue search or create new.
				// For simplicity, let's let this room be cleaned up and create/find another.
				// A more robust solution might involve a dedicated cleanup goroutine for stale rooms.
				// For now, we'll proceed, and if this user1's room is reset, this new player won't join it.
				// The reset logic needs to handle removing the room from the global 'rooms' slice.
				// Let's assume room.reset() handles this.
				// To avoid issues with modifying 'rooms' while iterating, mark for removal or rebuild list.
				// The current room.reset() calls removeRoomFromRooms which rebuilds 'rooms'.
				// This is problematic if called while iterating 'rooms'.
				// A safer pattern: collect rooms to reset, then reset them outside the loop.
				// Or, if room.reset() is called, it must be the last operation for that room in this function.
				// Given the current structure, if user1.ws is dead, that room is unusable.
				// We should probably close ws (the new connection) if we can't find a valid spot.
				// Let's refine this: if user1 disconnected, that room is stale.
				// We should clean it up.
				// For now, if user1 is disconnected, we skip this room.
				// The room will be reset when user1's handler eventually fails.
				log.Printf("User1 in a waiting room (nickname: %s) seems disconnected. Skipping.", room.user1.nickname)
				continue // Skip this stale room
			}

			if IsWebSocketConnected(ws) { // Check if the new player (user2) is still connected
				room.user2.check = true
				room.user2.ws = ws
				room.user2.nickname = nickname
				log.Printf("User %s (%s) joined User %s (%s) in a room.", nickname, ws.RemoteAddr(), room.user1.nickname, room.user1.ws.RemoteAddr())

				// Unlock globalMutex before starting MessageHandler as it's a long-running blocking call
				// and MessageHandler itself will manage room state.
				// However, MessageHandler might call room.reset() which modifies global 'rooms'.
				// This needs careful thought. If MessageHandler is run in a new goroutine,
				// the lock management changes.
				// For now, assuming MessageHandler is blocking and handles its own lifecycle.
				// The issue is that room.reset() modifies global 'rooms'.
				// Let's run MessageHandler in a goroutine.
				go room.MessageHandler()
				return // Successfully matched
			}
			// New player disconnected before matching completed
			log.Printf("New player %s (%s) disconnected before matching could complete.", nickname, ws.RemoteAddr())
			// No need to call handleConnectionFailure for ws, as it's already disconnected.
			// The IsWebSocketConnected check would have confirmed this.
			// We just return, global connectionsCount was already incremented and will be decremented if ws's handler fails.
			// Or, more accurately, if ws is now considered "failed to match", decrement count.
			// connectionsCount was incremented at the start. If ws fails here, it should be decremented.
			// This path means ws is dead.
			// We need a way to decrement connectionsCount for 'ws' if it dies here.
			// The IsWebSocketConnected(ws) implies it's dead.
			// Let's ensure handleConnectionFailure is robust.
			// The original handleFailedRoomMatching(ws) was for this.
			// It's better to call a generic cleanup for 'ws'.
			handleConnectionFailure(ws) // This will close and decrement count for ws.
			return
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
			} // Opponent (user1) timed out
			if room.user1.ws != nil {
				room.user1.ws.WriteJSON(Message{Message: StatusLoss})
			} // You (user1) timed out -> loss
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
			// Notify user1 of invalid move? Or just reset.
			// For simplicity, reset. A more advanced server might send an error and allow retry.
			room.reset()
			return
		}

		// White's turn (user2)
		currentMoveIndex, timeout, readErr = reading(room.user2.ws)
		if timeout {
			log.Printf("User %s (white) timed out. %s (black) wins.", room.user2.nickname, room.user1.nickname)
			if room.user1.ws != nil {
				room.user1.ws.WriteJSON(Message{Message: StatusUser2Timeout})
			} // Opponent (user2) timed out
			if room.user2.ws != nil {
				room.user2.ws.WriteJSON(Message{Message: StatusLoss})
			} // You (user2) timed out -> loss
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

	moveIndex, convErr := strconv.Atoi(string(msgBytes))
	if convErr != nil {
		log.Printf("Error converting message to int from %s ('%s'): %v", ws.RemoteAddr(), string(msgBytes), convErr)
		return 0, false, true // Conversion error
	}
	return moveIndex, false, false
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

	room.prepareForReset() // Close WebSockets and clear user checks

	globalMutex.Lock()
	defer globalMutex.Unlock()

	// Remove user WebSockets from global 'sockets' list and decrement count
	// This is tricky because handleConnectionFailure also does this.
	// A connection should only be removed and counted once.
	// Let's assume that if a room is reset, its player connections are "terminated" here.
	if room.user1.ws != nil { // This check is now redundant due to prepareForReset, but safe
		// removeWebSocketFromSockets(room.user1.ws) // Done by handleConnectionFailure if called from ws handler
		// connectionsCount-- // This logic needs to be robust against double counting
	}
	if room.user2.ws != nil {
		// removeWebSocketFromSockets(room.user2.ws)
		// connectionsCount--
	}
	// The connectionsCount for players is tricky. It's incremented when they first connect.
	// It should be decremented when their connection truly ends.
	// If room.reset() is called, it means the game ended. Their individual socket handlers
	// (SocketHandler -> RoomMatching -> which then might lead to MessageHandler)
	// will eventually terminate. The defer in those handlers (if any) or explicit calls
	// to handleConnectionFailure should manage the count.
	// For now, room.reset() focuses on cleaning the room's state and player slots.
	// The global 'connectionsCount' is managed by handleConnectionFailure.

	removeRoomFromRoomsUnsafe(room)        // Unsafe because it modifies 'rooms' without its own lock here (caller holds globalMutex)
	currentConnections := connectionsCount // Get current count under lock

	// No, BroadcastConnectionsCount should not be called with globalMutex held if it tries to acquire it.
	// Let's read count, unlock, then broadcast.
	// However, BroadcastConnectionsCount iterates 'sockets' which also needs globalMutex.
	// So, it's fine to call it while holding globalMutex.
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

	removeWebSocketFromSocketsUnsafe(ws)
	connectionsCount--
	currentConnections := connectionsCount
	BroadcastConnectionsCountUnsafe(currentConnections)
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
	// defer handleConnectionFailure(ws) // This defer will run when SocketHandler returns.
	// RoomMatching is blocking or spawns its own goroutine (MessageHandler).
	// If RoomMatching is blocking and ws is passed, then when RoomMatching returns,
	// this defer is appropriate.
	// If MessageHandler is a goroutine, ws might live on.
	// Let's make RoomMatching responsible for the lifecycle of ws if it takes it over.
	// Or, handleConnectionFailure should be robustly called when ws is truly done.

	// For a player, RoomMatching will handle its lifecycle.
	// If RoomMatching fails to match or the game ends, the connection should be cleaned up.
	// The current RoomMatching calls handleConnectionFailure on early exit.
	// If a game starts (MessageHandler), that goroutine is responsible for the player WebSockets.
	// The 'defer handleConnectionFailure(ws)' here would be for the case where upgradeWebSocketConnection succeeds
	// but RoomMatching itself panics or returns without assigning ws to a room or handling its closure.
	// This is a safety net.
	defer func() {
		// Check if ws is still in global sockets list. If so, it means it wasn't properly cleaned up.
		// This is a fallback. Proper cleanup should happen in game logic / room reset.
		globalMutex.Lock()
		stillExists := false
		for _, s := range sockets {
			if s == ws {
				stillExists = true
				break
			}
		}
		globalMutex.Unlock()

		if stillExists {
			log.Printf("Fallback cleanup for player WebSocket %s in SocketHandler defer", ws.RemoteAddr())
			handleConnectionFailure(ws)
		}
	}()

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

		for {
			if !IsWebSocketConnected(spectatorWs) { // Check if spectator is still connected
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
	// Set a short deadline for the write operation itself.
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	err := conn.WriteJSON(map[string]interface{}{"type": WebSocketPingType})
	conn.SetWriteDeadline(time.Time{}) // Clear write deadline

	if err != nil {
		log.Printf("IsWebSocketConnected: Failed to send Ping to %s: %v", conn.RemoteAddr(), err)
		return false
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second)) // Expect pong within 3 seconds
	msgType, pongMsg, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{}) // Clear read deadline

	if err != nil {
		// Don't log "normal" timeouts if that's how we detect closure via ping.
		// if e, ok := err.(net.Error); !ok || !e.Timeout() {
		// log.Printf("IsWebSocketConnected: Failed to receive Pong from %s: %v", conn.RemoteAddr(), err)
		// }
		return false
	}

	if msgType == websocket.TextMessage && string(pongMsg) == WebSocketPongType {
		return true
	}
	// log.Printf("IsWebSocketConnected: Received unexpected Pong from %s (type %d, content '%s')", conn.RemoteAddr(), msgType, string(pongMsg))
	return false // Or true if any message is considered a sign of life after ping. Strict check for now.
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

// Main function
func main() {
	http.HandleFunc("/game", SocketHandler)
	http.HandleFunc("/spectator", SpectatorHandler)

	log.Println("Omok server starting on", serverAddress)
	if err := http.ListenAndServe(serverAddress, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
