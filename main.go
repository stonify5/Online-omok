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

const serverAddress = "0.0.0.0:8080"

const (
	BoardSize         = 15
	TotalCells        = BoardSize * BoardSize
	MaxNicknameLength = 10
	TimeoutDuration   = 60 * time.Second
	WinningCount      = 5
)

const (
	black   uint8 = 1
	white   uint8 = 2
	emptied uint8 = 0
)

const (
	StatusWin          = 0
	StatusLoss         = 1
	StatusUser2Timeout = 2
	StatusUser1Timeout = 3
	StatusErrorReading = 4
)

const (
	WebSocketPingType = "ping"
	WebSocketPongType = "pong"
)

type OmokRoom struct {
	board_15x15   [TotalCells]uint8
	user1         user
	user2         user
	spectators    []*websocket.Conn
	spectatorsMux sync.Mutex
}

type user struct {
	ws       *websocket.Conn
	check    bool
	nickname string
}

type Message struct {
	Data      interface{} `json:"data,omitempty"`
	YourColor interface{} `json:"YourColor,omitempty"`
	Message   interface{} `json:"message,omitempty"`
	NumUsers  interface{} `json:"numUsers,omitempty"`
	Nickname  interface{} `json:"nickname,omitempty"`
}

type SpectatorMessage struct {
	Board interface{} `json:"board,omitempty"`
	Data  interface{} `json:"data,omitempty"`
	Color interface{} `json:"color,omitempty"`
	User1 interface{} `json:"user1,omitempty"`
	User2 interface{} `json:"user2,omitempty"`
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	rooms            []*OmokRoom
	sockets          []*websocket.Conn
	globalMutex      sync.Mutex
	connectionsCount = 0
)

func RoomMatching(ws *websocket.Conn) {
	clientAddr := ws.RemoteAddr().String()
	log.Printf("[ROOM_MATCHING] [START] client=%s action=connection_attempt", clientAddr)

	if ws == nil {
		log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=websocket_nil", clientAddr)
		return
	}

	log.Printf("[ROOM_MATCHING] [INFO] client=%s action=websocket_verified status=reading_nickname", clientAddr)

	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	messageType, nicknameBytes, err := ws.ReadMessage()
	ws.SetReadDeadline(time.Time{})

	log.Printf("[ROOM_MATCHING] [DEBUG] client=%s action=read_message message_type=%d bytes_length=%d error=%v",
		clientAddr, messageType, len(nicknameBytes), err)

	if err != nil {
		log.Printf("[ROOM_MATCHING] [ERROR] client=%s action=read_nickname error=%v", clientAddr, err)
		if netErr, ok := err.(net.Error); ok {
			if netErr.Timeout() {
				log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=timeout error_type=network_timeout", clientAddr)
			} else {
				log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=network_error details=%v", clientAddr, netErr)
			}
		} else {
			log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=non_network_error details=%v", clientAddr, err)
		}
		handleConnectionFailure(ws)
		return
	}

	if len(nicknameBytes) == 0 {
		log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=empty_message bytes_received=0", clientAddr)
		handleConnectionFailure(ws)
		return
	}

	nickname := string(nicknameBytes)
	log.Printf("[ROOM_MATCHING] [DEBUG] client=%s action=nickname_received raw_nickname=%q bytes_length=%d",
		clientAddr, nickname, len(nicknameBytes))

	var jsonMsg map[string]interface{}
	if json.Unmarshal(nicknameBytes, &jsonMsg) == nil {
		log.Printf("[ROOM_MATCHING] [DEBUG] client=%s action=json_parsing status=success", clientAddr)
		if nick, exists := jsonMsg["nickname"]; exists {
			if nickStr, ok := nick.(string); ok {
				nickname = nickStr
				log.Printf("[ROOM_MATCHING] [INFO] client=%s action=nickname_extracted nickname=%s", clientAddr, nickname)
			}
		} else {
			log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=json_no_nickname_field json_data=%v", clientAddr, jsonMsg)
			handleConnectionFailure(ws)
			return
		}
	} else {
		log.Printf("[ROOM_MATCHING] [DEBUG] client=%s action=json_parsing status=failed using_raw_string", clientAddr)
	}

	nicknameLength := utf8.RuneCountInString(nickname)
	log.Printf("[ROOM_MATCHING] [DEBUG] client=%s action=nickname_validation nickname=%s length=%d",
		clientAddr, nickname, nicknameLength)

	if nicknameLength == 0 {
		log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=empty_nickname", clientAddr)
		handleConnectionFailure(ws)
		return
	}
	if nicknameLength > MaxNicknameLength {
		log.Printf("[ROOM_MATCHING] [ERROR] client=%s error=nickname_too_long length=%d max_length=%d",
			clientAddr, nicknameLength, MaxNicknameLength)
		handleConnectionFailure(ws)
		return
	}

	log.Printf("[ROOM_MATCHING] [INFO] client=%s action=nickname_validated nickname=%s status=valid", clientAddr, nickname)

	globalMutex.Lock()
	defer globalMutex.Unlock()

	log.Printf("[ROOM_MATCHING] [INFO] client=%s nickname=%s action=entering_critical_section", clientAddr, nickname)
	cleanupDisconnectedWaitingPlayers()
	log.Printf("[ROOM_MATCHING] [DEBUG] client=%s nickname=%s action=cleanup_completed current_rooms=%d",
		clientAddr, nickname, len(rooms))

	for i, room := range rooms {
		if room.user1.check && !room.user2.check {
			log.Printf("[ROOM_MATCHING] [DEBUG] client=%s nickname=%s action=found_waiting_room room_index=%d waiting_user=%s",
				clientAddr, nickname, i, room.user1.nickname)

			if room.user1.ws != nil && IsWebSocketConnectedSync(room.user1.ws) {
				room.user2.check = true
				room.user2.ws = ws
				room.user2.nickname = nickname
				log.Printf("[ROOM_MATCHING] [SUCCESS] client=%s nickname=%s action=joined_room partner=%s partner_addr=%s",
					nickname, clientAddr, room.user1.nickname, room.user1.ws.RemoteAddr().String())

				connectionsCount++
				currentConnections := connectionsCount
				log.Printf("[ROOM_MATCHING] [INFO] client=%s nickname=%s action=connection_count_increment role=user2 current_total=%d",
					clientAddr, nickname, currentConnections)
				BroadcastConnectionsCountUnsafe(currentConnections)

				log.Printf("[ROOM_MATCHING] [INFO] client=%s nickname=%s action=starting_game", clientAddr, nickname)
				go room.MessageHandler()
				return
			} else {
				log.Printf("[ROOM_MATCHING] [WARN] client=%s nickname=%s action=removing_disconnected_room waiting_user=%s",
					clientAddr, nickname, room.user1.nickname)
				if room.user1.ws != nil {
					removeWebSocketFromSocketsUnsafe(room.user1.ws)
					room.user1.ws.Close()
				}
			}
		}
	}

	newRoom := &OmokRoom{}
	newRoom.user1.check = true
	newRoom.user1.ws = ws
	newRoom.user1.nickname = nickname
	rooms = append(rooms, newRoom)
	log.Printf("[ROOM_MATCHING] [INFO] client=%s nickname=%s action=created_new_room role=user1 total_rooms=%d",
		clientAddr, nickname, len(rooms))

	connectionsCount++
	currentConnections := connectionsCount
	log.Printf("[ROOM_MATCHING] [INFO] client=%s nickname=%s action=connection_count_increment role=user1 current_total=%d",
		clientAddr, nickname, currentConnections)
	BroadcastConnectionsCountUnsafe(currentConnections)

	waitingMsg := Message{Message: "Waiting for opponent..."}
	if err := ws.WriteJSON(waitingMsg); err != nil {
		log.Printf("[ROOM_MATCHING] [ERROR] client=%s nickname=%s action=send_waiting_message error=%v",
			clientAddr, nickname, err)
	} else {
		log.Printf("[ROOM_MATCHING] [INFO] client=%s nickname=%s action=send_waiting_message status=success",
			clientAddr, nickname)
	}
}
func (room *OmokRoom) MessageHandler() {
	user1Nick := room.user1.nickname
	user2Nick := room.user2.nickname
	log.Printf("[GAME] [START] user1=%s user2=%s action=game_starting", user1Nick, user2Nick)

	err1 := room.user1.ws.WriteJSON(Message{YourColor: "black", Nickname: room.user2.nickname})
	err2 := room.user2.ws.WriteJSON(Message{YourColor: "white", Nickname: room.user1.nickname})

	if err1 != nil {
		log.Printf("[GAME] [ERROR] user1=%s user2=%s action=send_initial_message target=user1 error=%v", user1Nick, user2Nick, err1)
	}
	if err2 != nil {
		log.Printf("[GAME] [ERROR] user1=%s user2=%s action=send_initial_message target=user2 error=%v", user1Nick, user2Nick, err2)
	}

	if err1 != nil || err2 != nil {
		log.Printf("[GAME] [ERROR] user1=%s user2=%s action=initial_message_failed status=resetting_room", user1Nick, user2Nick)
		room.reset()
		return
	}

	log.Printf("[GAME] [INFO] user1=%s user2=%s action=initial_messages_sent status=game_loop_starting", user1Nick, user2Nick)

	var currentMoveIndex int
	var timeout bool
	var readErr bool
	moveCount := 0

	for {
		moveCount++
		log.Printf("[GAME] [TURN] user1=%s user2=%s turn=black move_count=%d action=waiting_for_move", user1Nick, user2Nick, moveCount)

		currentMoveIndex, timeout, readErr = reading(room.user1.ws)
		if timeout {
			log.Printf("[GAME] [END] user1=%s user2=%s turn=black result=timeout winner=%s reason=user1_timeout", user1Nick, user2Nick, user2Nick)
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
			log.Printf("[GAME] [END] user1=%s user2=%s turn=black result=read_error winner=%s reason=user1_read_error", user1Nick, user2Nick, user2Nick)
			if room.user2.ws != nil {
				room.user2.ws.WriteJSON(Message{Message: StatusErrorReading})
			}
			room.reset()
			return
		}

		log.Printf("[GAME] [MOVE] user1=%s user2=%s turn=black move_index=%d action=validating_move", user1Nick, user2Nick, currentMoveIndex)

		if room.isValidMove(currentMoveIndex) {
			room.board_15x15[currentMoveIndex] = black
			log.Printf("[GAME] [MOVE] user1=%s user2=%s turn=black move_index=%d action=move_applied status=valid", user1Nick, user2Nick, currentMoveIndex)

			if room.user2.ws != nil {
				if err := room.user2.ws.WriteJSON(Message{Data: currentMoveIndex}); err != nil {
					log.Printf("[GAME] [ERROR] user1=%s user2=%s turn=black action=send_move_to_opponent error=%v", user1Nick, user2Nick, err)
					room.reset()
					return
				}
			}
			room.broadcastMoveToSpectators(currentMoveIndex, black)

			if room.VictoryConfirm(currentMoveIndex) {
				log.Printf("[GAME] [END] user1=%s user2=%s turn=black result=victory winner=%s move_index=%d", user1Nick, user2Nick, user1Nick, currentMoveIndex)
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
			log.Printf("[GAME] [END] user1=%s user2=%s turn=black result=invalid_move move_index=%d action=resetting", user1Nick, user2Nick, currentMoveIndex)
			room.reset()
			return
		}

		log.Printf("[GAME] [TURN] user1=%s user2=%s turn=white move_count=%d action=waiting_for_move", user1Nick, user2Nick, moveCount)

		currentMoveIndex, timeout, readErr = reading(room.user2.ws)
		if timeout {
			log.Printf("[GAME] [END] user1=%s user2=%s turn=white result=timeout winner=%s reason=user2_timeout", user1Nick, user2Nick, user1Nick)
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
			log.Printf("[GAME] [END] user1=%s user2=%s turn=white result=read_error winner=%s reason=user2_read_error", user1Nick, user2Nick, user1Nick)
			if room.user1.ws != nil {
				room.user1.ws.WriteJSON(Message{Message: StatusErrorReading})
			}
			room.reset()
			return
		}

		log.Printf("[GAME] [MOVE] user1=%s user2=%s turn=white move_index=%d action=validating_move", user1Nick, user2Nick, currentMoveIndex)

		if room.isValidMove(currentMoveIndex) {
			room.board_15x15[currentMoveIndex] = white
			log.Printf("[GAME] [MOVE] user1=%s user2=%s turn=white move_index=%d action=move_applied status=valid", user1Nick, user2Nick, currentMoveIndex)

			if room.user1.ws != nil {
				if err := room.user1.ws.WriteJSON(Message{Data: currentMoveIndex}); err != nil {
					log.Printf("[GAME] [ERROR] user1=%s user2=%s turn=white action=send_move_to_opponent error=%v", user1Nick, user2Nick, err)
					room.reset()
					return
				}
			}
			room.broadcastMoveToSpectators(currentMoveIndex, white)

			if room.VictoryConfirm(currentMoveIndex) {
				log.Printf("[GAME] [END] user1=%s user2=%s turn=white result=victory winner=%s move_index=%d", user1Nick, user2Nick, user2Nick, currentMoveIndex)
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
			log.Printf("[GAME] [END] user1=%s user2=%s turn=white result=invalid_move move_index=%d action=resetting", user1Nick, user2Nick, currentMoveIndex)
			room.reset()
			return
		}
	}
}

func (room *OmokRoom) isValidMove(index int) bool {
	isValid := index >= 0 && index < TotalCells && room.board_15x15[index] == emptied
	log.Printf("[GAME] [VALIDATION] action=move_validation move_index=%d is_valid=%t board_value=%d",
		index, isValid, room.board_15x15[index])
	return isValid
}

func (room *OmokRoom) broadcastMoveToSpectators(moveIndex int, color uint8) {
	room.spectatorsMux.Lock()
	defer room.spectatorsMux.Unlock()

	spectatorCount := len(room.spectators)
	colorStr := "black"
	if color == white {
		colorStr = "white"
	}

	log.Printf("[SPECTATOR] [BROADCAST] action=move_broadcast move_index=%d color=%s spectator_count=%d",
		moveIndex, colorStr, spectatorCount)

	if spectatorCount == 0 {
		log.Printf("[SPECTATOR] [BROADCAST] action=no_spectators move_index=%d", moveIndex)
		return
	}

	message := SpectatorMessage{Data: moveIndex, Color: color}
	successCount := 0
	errorCount := 0

	for i, spectatorWs := range room.spectators {
		if spectatorWs != nil {
			if err := spectatorWs.WriteJSON(message); err != nil {
				log.Printf("[SPECTATOR] [ERROR] action=broadcast_failed spectator_index=%d addr=%s error=%v",
					i, spectatorWs.RemoteAddr(), err)
				errorCount++
			} else {
				successCount++
			}
		} else {
			log.Printf("[SPECTATOR] [WARN] action=nil_spectator_socket spectator_index=%d", i)
		}
	}

	log.Printf("[SPECTATOR] [BROADCAST] action=broadcast_complete move_index=%d success=%d errors=%d",
		moveIndex, successCount, errorCount)
}

func (room *OmokRoom) VictoryConfirm(index int) bool {
	stoneColor := room.board_15x15[index]
	colorStr := "empty"
	if stoneColor == black {
		colorStr = "black"
	} else if stoneColor == white {
		colorStr = "white"
	}

	log.Printf("[GAME] [VICTORY_CHECK] action=victory_check_start move_index=%d color=%s", index, colorStr)

	if stoneColor == emptied {
		log.Printf("[GAME] [VICTORY_CHECK] action=victory_check_failed move_index=%d reason=empty_cell", index)
		return false
	}

	checkDirections := [][2]int{
		{0, 1},  // horizontal
		{1, 0},  // vertical
		{1, 1},  // diagonal \
		{1, -1}, // diagonal /
	}

	y := index / BoardSize
	x := index % BoardSize
	log.Printf("[GAME] [VICTORY_CHECK] action=position_calculated move_index=%d x=%d y=%d", index, x, y)

	for dirIndex, dir := range checkDirections {
		dy, dx := dir[0], dir[1]
		count := 1

		directionName := []string{"horizontal", "vertical", "diagonal_down", "diagonal_up"}[dirIndex]
		log.Printf("[GAME] [VICTORY_CHECK] action=checking_direction move_index=%d direction=%s dx=%d dy=%d",
			index, directionName, dx, dy)

		// Check positive direction
		for i := 1; i < WinningCount; i++ {
			curX, curY := x+dx*i, y+dy*i
			if curX >= 0 && curX < BoardSize && curY >= 0 && curY < BoardSize &&
				room.board_15x15[curY*BoardSize+curX] == stoneColor {
				count++
			} else {
				break
			}
		}

		// Check negative direction
		for i := 1; i < WinningCount; i++ {
			curX, curY := x-dx*i, y-dy*i
			if curX >= 0 && curX < BoardSize && curY >= 0 && curY < BoardSize &&
				room.board_15x15[curY*BoardSize+curX] == stoneColor {
				count++
			} else {
				break
			}
		}

		log.Printf("[GAME] [VICTORY_CHECK] action=direction_checked move_index=%d direction=%s count=%d needed=%d",
			index, directionName, count, WinningCount)

		if count >= WinningCount {
			log.Printf("[GAME] [VICTORY_CHECK] action=victory_confirmed move_index=%d direction=%s count=%d color=%s",
				index, directionName, count, colorStr)
			return true
		}
	}

	log.Printf("[GAME] [VICTORY_CHECK] action=victory_check_complete move_index=%d result=no_victory color=%s",
		index, colorStr)
	return false
}

func (room *OmokRoom) SendVictoryMessage(winnerColor uint8) {
	if winnerColor == black {
		if room.user1.ws != nil {
			room.user1.ws.WriteJSON(Message{Message: StatusWin})
		}
		if room.user2.ws != nil {
			room.user2.ws.WriteJSON(Message{Message: StatusLoss})
		}
	} else {
		if room.user2.ws != nil {
			room.user2.ws.WriteJSON(Message{Message: StatusWin})
		}
		if room.user1.ws != nil {
			room.user1.ws.WriteJSON(Message{Message: StatusLoss})
		}
	}
}

func reading(ws *websocket.Conn) (int, bool, bool) {
	if ws == nil {
		log.Printf("[GAME] [READ] action=read_failed reason=nil_websocket")
		return 0, false, true
	}

	clientAddr := ws.RemoteAddr().String()
	log.Printf("[GAME] [READ] action=read_start client=%s timeout=%v", clientAddr, TimeoutDuration)

	for {
		ws.SetReadDeadline(time.Now().Add(TimeoutDuration))
		_, msgBytes, err := ws.ReadMessage()
		ws.SetReadDeadline(time.Time{})

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("[GAME] [READ] action=read_timeout client=%s timeout_duration=%v", clientAddr, TimeoutDuration)
				return 0, true, false
			}
			log.Printf("[GAME] [READ] action=read_error client=%s error=%v", clientAddr, err)
			return 0, false, true
		}

		msgStr := string(msgBytes)
		log.Printf("[GAME] [READ] action=message_received client=%s message=%q bytes_length=%d",
			clientAddr, msgStr, len(msgBytes))

		if msgStr == WebSocketPongType {
			log.Printf("[GAME] [READ] action=pong_received client=%s status=continuing", clientAddr)
			continue
		}

		var jsonMsg map[string]interface{}
		if json.Unmarshal(msgBytes, &jsonMsg) == nil {
			log.Printf("[GAME] [READ] action=json_parsed client=%s json_data=%v", clientAddr, jsonMsg)
			if msgType, exists := jsonMsg["type"]; exists && msgType == WebSocketPingType {
				log.Printf("[GAME] [READ] action=ping_received client=%s status=sending_pong", clientAddr)
				ws.WriteJSON(map[string]interface{}{"type": WebSocketPongType})
				continue
			}
		} else {
			log.Printf("[GAME] [READ] action=json_parse_failed client=%s using_raw_string", clientAddr)
		}

		moveIndex, convErr := strconv.Atoi(msgStr)
		if convErr != nil {
			log.Printf("[GAME] [READ] action=move_conversion_failed client=%s message=%q error=%v",
				clientAddr, msgStr, convErr)
			return 0, false, true
		}

		log.Printf("[GAME] [READ] action=move_parsed client=%s move_index=%d status=success",
			clientAddr, moveIndex)
		return moveIndex, false, false
	}
}
func (room *OmokRoom) prepareForReset() {
	user1Nick := room.user1.nickname
	user2Nick := room.user2.nickname
	log.Printf("[ROOM] [RESET] action=prepare_for_reset user1=%s user2=%s", user1Nick, user2Nick)

	if room.user1.ws != nil {
		log.Printf("[ROOM] [RESET] action=closing_user1_ws user1=%s addr=%s", user1Nick, room.user1.ws.RemoteAddr())
		room.user1.ws.Close()
	}
	if room.user2.ws != nil {
		log.Printf("[ROOM] [RESET] action=closing_user2_ws user2=%s addr=%s", user2Nick, room.user2.ws.RemoteAddr())
		room.user2.ws.Close()
	}
	room.user1.ws = nil
	room.user2.ws = nil
	room.user1.check = false
	room.user2.check = false

	room.spectatorsMux.Lock()
	spectatorCount := len(room.spectators)
	log.Printf("[ROOM] [RESET] action=closing_spectators user1=%s user2=%s spectator_count=%d",
		user1Nick, user2Nick, spectatorCount)
	for i, specWs := range room.spectators {
		if specWs != nil {
			log.Printf("[ROOM] [RESET] action=closing_spectator spectator_index=%d addr=%s",
				i, specWs.RemoteAddr())
			specWs.Close()
		}
	}
	room.spectators = nil
	room.spectatorsMux.Unlock()
	log.Printf("[ROOM] [RESET] action=spectators_closed user1=%s user2=%s", user1Nick, user2Nick)
}

func (room *OmokRoom) reset() {
	user1Nick := room.user1.nickname
	user2Nick := room.user2.nickname
	log.Printf("[ROOM] [RESET] action=reset_start user1=%s user2=%s", user1Nick, user2Nick)

	user1WS := room.user1.ws
	user2WS := room.user2.ws
	user1Active := room.user1.check
	user2Active := room.user2.check

	log.Printf("[ROOM] [RESET] action=captured_state user1=%s user2=%s user1_active=%t user2_active=%t user1_ws_nil=%t user2_ws_nil=%t",
		user1Nick, user2Nick, user1Active, user2Active, (user1WS == nil), (user2WS == nil))

	room.prepareForReset()

	globalMutex.Lock()
	defer globalMutex.Unlock()

	connectionsDecremented := 0

	if user1Active && user1WS != nil {
		wasInList := false
		for _, socket := range sockets {
			if socket == user1WS {
				wasInList = true
				break
			}
		}
		if wasInList {
			removeWebSocketFromSocketsUnsafe(user1WS)
			connectionsCount--
			connectionsDecremented++
			log.Printf("[ROOM] [RESET] action=user1_connection_decremented user1=%s connections_count=%d",
				user1Nick, connectionsCount)
		} else {
			log.Printf("[ROOM] [RESET] action=user1_already_removed user1=%s reason=not_in_global_list", user1Nick)
		}
	} else {
		log.Printf("[ROOM] [RESET] action=user1_skip_decrement user1=%s user1_active=%t user1_ws_nil=%t",
			user1Nick, user1Active, (user1WS == nil))
	}

	if user2Active && user2WS != nil {
		wasInList := false
		for _, socket := range sockets {
			if socket == user2WS {
				wasInList = true
				break
			}
		}
		if wasInList {
			removeWebSocketFromSocketsUnsafe(user2WS)
			connectionsCount--
			connectionsDecremented++
			log.Printf("[ROOM] [RESET] action=user2_connection_decremented user2=%s connections_count=%d",
				user2Nick, connectionsCount)
		} else {
			log.Printf("[ROOM] [RESET] action=user2_already_removed user2=%s reason=not_in_global_list", user2Nick)
		}
	} else {
		log.Printf("[ROOM] [RESET] action=user2_skip_decrement user2=%s user2_active=%t user2_ws_nil=%t",
			user2Nick, user2Active, (user2WS == nil))
	}

	removeRoomFromRoomsUnsafe(room)
	currentConnections := connectionsCount

	if connectionsDecremented > 0 {
		log.Printf("[ROOM] [RESET] action=reset_complete user1=%s user2=%s decremented=%d current_total=%d status=broadcasting",
			user1Nick, user2Nick, connectionsDecremented, currentConnections)
		BroadcastConnectionsCountUnsafe(currentConnections)
	} else {
		log.Printf("[ROOM] [RESET] action=reset_complete user1=%s user2=%s decremented=0 current_total=%d status=no_broadcast",
			user1Nick, user2Nick, currentConnections)
	}
}
func handleConnectionFailure(ws *websocket.Conn) {
	if ws == nil {
		log.Printf("[CONNECTION] [FAILURE] action=handle_failure status=nil_websocket")
		return
	}

	clientAddr := ws.RemoteAddr().String()
	log.Printf("[CONNECTION] [FAILURE] action=handle_failure client=%s", clientAddr)

	globalMutex.Lock()
	defer globalMutex.Unlock()

	wasInList := false
	newSockets := []*websocket.Conn{}
	socketsBefore := len(sockets)

	for _, socket := range sockets {
		if socket == ws {
			wasInList = true
			log.Printf("[CONNECTION] [FAILURE] action=socket_found_in_list client=%s", clientAddr)
		} else {
			newSockets = append(newSockets, socket)
		}
	}

	if wasInList {
		sockets = newSockets
		connectionsCount--
		socketsAfter := len(sockets)
		log.Printf("[CONNECTION] [FAILURE] action=socket_removed client=%s sockets_before=%d sockets_after=%d connections=%d",
			clientAddr, socketsBefore, socketsAfter, connectionsCount)
		BroadcastConnectionsCountUnsafe(connectionsCount)
	} else {
		log.Printf("[CONNECTION] [FAILURE] action=socket_not_found client=%s status=already_removed", clientAddr)
	}

	log.Printf("[CONNECTION] [FAILURE] action=closing_websocket client=%s", clientAddr)
	ws.Close()
	log.Printf("[CONNECTION] [FAILURE] action=failure_handled client=%s", clientAddr)
}

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

func BroadcastConnectionsCountUnsafe(count int) {
	initialSocketCount := len(sockets)
	log.Printf("[BROADCAST] [CONNECTION_COUNT] action=broadcast_start count=%d target_sockets=%d",
		count, initialSocketCount)

	validSockets := []*websocket.Conn{}
	message := Message{NumUsers: count}
	successCount := 0
	errorCount := 0

	for i, s := range sockets {
		if s != nil {
			err := s.WriteJSON(message)
			if err != nil {
				log.Printf("[BROADCAST] [CONNECTION_COUNT] action=broadcast_failed socket_index=%d addr=%s error=%v",
					i, s.RemoteAddr(), err)
				s.Close()
				errorCount++
			} else {
				validSockets = append(validSockets, s)
				successCount++
			}
		} else {
			log.Printf("[BROADCAST] [CONNECTION_COUNT] action=nil_socket socket_index=%d", i)
		}
	}

	socketsRemoved := len(sockets) - len(validSockets)
	if socketsRemoved > 0 {
		sockets = validSockets
		connectionsCount = len(sockets)
		log.Printf("[BROADCAST] [CONNECTION_COUNT] action=cleanup_invalid_sockets removed=%d new_count=%d",
			socketsRemoved, connectionsCount)
	}

	log.Printf("[BROADCAST] [CONNECTION_COUNT] action=broadcast_complete count=%d success=%d errors=%d final_sockets=%d",
		count, successCount, errorCount, len(sockets))
}

func BroadcastConnectionsCount(count int) {
	log.Printf("[BROADCAST] [CONNECTION_COUNT] action=broadcast_with_lock count=%d", count)
	globalMutex.Lock()
	defer globalMutex.Unlock()
	BroadcastConnectionsCountUnsafe(count)
}
func upgradeWebSocketConnection(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	clientAddr := r.RemoteAddr
	log.Printf("[WEBSOCKET] [UPGRADE] action=upgrade_attempt client=%s", clientAddr)

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WEBSOCKET] [UPGRADE] action=upgrade_failed client=%s error=%v", clientAddr, err)
		return nil, err
	}

	wsAddr := ws.RemoteAddr().String()
	log.Printf("[WEBSOCKET] [UPGRADE] action=upgrade_success client=%s ws_addr=%s", clientAddr, wsAddr)

	globalMutex.Lock()
	sockets = append(sockets, ws)
	totalSockets := len(sockets)
	globalMutex.Unlock()

	log.Printf("[WEBSOCKET] [UPGRADE] action=added_to_sockets client=%s total_sockets=%d", wsAddr, totalSockets)

	testErr := ws.WriteJSON(map[string]interface{}{
		"type":    "connection_established",
		"message": "WebSocket connection ready",
	})
	if testErr != nil {
		log.Printf("[WEBSOCKET] [UPGRADE] action=test_message_failed client=%s error=%v", wsAddr, testErr)
		return ws, nil
	}

	log.Printf("[WEBSOCKET] [UPGRADE] action=test_message_success client=%s status=ready", wsAddr)
	return ws, nil
}

func SocketHandler(w http.ResponseWriter, r *http.Request) {
	clientAddr := r.RemoteAddr
	log.Printf("[HANDLER] [GAME] action=connection_request client=%s", clientAddr)

	userAgent := r.Header.Get("User-Agent")
	origin := r.Header.Get("Origin")
	log.Printf("[HANDLER] [GAME] action=request_details client=%s user_agent=%q origin=%q",
		clientAddr, userAgent, origin)

	ws, err := upgradeWebSocketConnection(w, r)
	if err != nil {
		log.Printf("[HANDLER] [GAME] action=upgrade_failed client=%s error=%v", clientAddr, err)
		return
	}

	wsAddr := ws.RemoteAddr().String()
	log.Printf("[HANDLER] [GAME] action=upgrade_success client=%s ws_addr=%s status=starting_room_matching",
		clientAddr, wsAddr)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[HANDLER] [GAME] action=panic_recovery client=%s panic=%v status=handling_failure",
					wsAddr, r)
				handleConnectionFailure(ws)
			}
		}()

		log.Printf("[HANDLER] [GAME] action=room_matching_start client=%s", wsAddr)
		RoomMatching(ws)
		log.Printf("[HANDLER] [GAME] action=room_matching_end client=%s", wsAddr)
	}()
}
func SpectatorHandler(w http.ResponseWriter, r *http.Request) {
	clientAddr := r.RemoteAddr
	log.Printf("[HANDLER] [SPECTATOR] action=connection_request client=%s", clientAddr)

	ws, err := upgradeWebSocketConnection(w, r)
	if err != nil {
		log.Printf("[HANDLER] [SPECTATOR] action=upgrade_failed client=%s error=%v", clientAddr, err)
		return
	}

	wsAddr := ws.RemoteAddr().String()
	log.Printf("[HANDLER] [SPECTATOR] action=upgrade_success client=%s ws_addr=%s", clientAddr, wsAddr)

	globalMutex.Lock()
	connectionsCount++
	currentConnections := connectionsCount
	globalMutex.Unlock()

	log.Printf("[HANDLER] [SPECTATOR] action=connection_count_increment client=%s current_total=%d",
		wsAddr, currentConnections)
	BroadcastConnectionsCount(currentConnections)

	go func(spectatorWs *websocket.Conn) {
		spectatorAddr := spectatorWs.RemoteAddr().String()
		log.Printf("[SPECTATOR] [SESSION] action=session_start client=%s status=finding_room", spectatorAddr)

		var assignedRoom *OmokRoom
		var lastSentBoardState [TotalCells]uint8
		connectionCheckCounter := 0

		defer func() {
			log.Printf("[SPECTATOR] [SESSION] action=session_cleanup client=%s", spectatorAddr)
			if assignedRoom != nil {
				log.Printf("[SPECTATOR] [SESSION] action=removing_from_room client=%s", spectatorAddr)
				removeSocketFromSpectators(assignedRoom, spectatorWs)
			}
			handleConnectionFailure(spectatorWs)
			log.Printf("[SPECTATOR] [SESSION] action=session_end client=%s", spectatorAddr)
		}()

		for {
			connectionCheckCounter++
			if connectionCheckCounter%10 == 0 {
				if !IsWebSocketConnected(spectatorWs) {
					log.Printf("[SPECTATOR] [SESSION] action=connection_lost client=%s check_count=%d",
						spectatorAddr, connectionCheckCounter)
					return
				}
				log.Printf("[SPECTATOR] [SESSION] action=connection_check client=%s check_count=%d status=alive",
					spectatorAddr, connectionCheckCounter)
			}

			globalMutex.Lock()
			foundRoomThisCycle := false
			roomsAvailable := len(rooms)
			log.Printf("[SPECTATOR] [SESSION] action=searching_rooms client=%s available_rooms=%d",
				spectatorAddr, roomsAvailable)

			for roomIndex, room := range rooms {
				if room.user1.check && room.user2.check {
					log.Printf("[SPECTATOR] [SESSION] action=found_active_room client=%s room_index=%d user1=%s user2=%s",
						spectatorAddr, roomIndex, room.user1.nickname, room.user2.nickname)
					if assignedRoom != room {
						if assignedRoom != nil {
							log.Printf("[SPECTATOR] [SESSION] action=leaving_previous_room client=%s previous_room_users=%s_%s",
								spectatorAddr, assignedRoom.user1.nickname, assignedRoom.user2.nickname)
							removeSocketFromSpectators(assignedRoom, spectatorWs)
						}
						assignedRoom = room
						addSocketToSpectators(assignedRoom, spectatorWs)
						log.Printf("[SPECTATOR] [SESSION] action=joined_new_room client=%s room_index=%d user1=%s user2=%s",
							spectatorAddr, roomIndex, room.user1.nickname, room.user2.nickname)

						assignedRoom.spectatorsMux.Lock()
						boardCopy := assignedRoom.board_15x15
						lastSentBoardState = boardCopy
						assignedRoom.spectatorsMux.Unlock()

						initialMsg := SpectatorMessage{
							Board: boardCopy,
							User1: room.user1.nickname,
							User2: room.user2.nickname,
						}
						if err := spectatorWs.WriteJSON(initialMsg); err != nil {
							log.Printf("[SPECTATOR] [SESSION] action=initial_state_send_failed client=%s error=%v",
								spectatorAddr, err)
							return
						}
						log.Printf("[SPECTATOR] [SESSION] action=initial_state_sent client=%s", spectatorAddr)
					} else {
						assignedRoom.spectatorsMux.Lock()
						boardChanged := false
						if assignedRoom.board_15x15 != lastSentBoardState {
							boardChanged = true
							lastSentBoardState = assignedRoom.board_15x15
						}
						boardCopy := lastSentBoardState
						assignedRoom.spectatorsMux.Unlock()

						if boardChanged {
							log.Printf("[SPECTATOR] [SESSION] action=board_changed client=%s status=sending_update", spectatorAddr)
							updateMsg := SpectatorMessage{Board: boardCopy}
							if err := spectatorWs.WriteJSON(updateMsg); err != nil {
								log.Printf("[SPECTATOR] [SESSION] action=board_update_send_failed client=%s error=%v",
									spectatorAddr, err)
								return
							}
							log.Printf("[SPECTATOR] [SESSION] action=board_update_sent client=%s", spectatorAddr)
						}
					}
					foundRoomThisCycle = true
					break
				}
			}
			globalMutex.Unlock()

			if !foundRoomThisCycle {
				if assignedRoom != nil {
					log.Printf("[SPECTATOR] [SESSION] action=game_ended client=%s previous_room_users=%s_%s status=searching_new",
						spectatorAddr, assignedRoom.user1.nickname, assignedRoom.user2.nickname)
					removeSocketFromSpectators(assignedRoom, spectatorWs)
					assignedRoom = nil
				} else {
					log.Printf("[SPECTATOR] [SESSION] action=no_games_available client=%s", spectatorAddr)
				}

				if err := spectatorWs.WriteJSON(Message{Message: "No active games to spectate. Waiting..."}); err != nil {
					log.Printf("[SPECTATOR] [SESSION] action=waiting_message_send_failed client=%s error=%v",
						spectatorAddr, err)
					return
				}
				log.Printf("[SPECTATOR] [SESSION] action=waiting_message_sent client=%s", spectatorAddr)
			}

			time.Sleep(2 * time.Second)
		}
	}(ws)
}

func IsWebSocketConnected(conn *websocket.Conn) bool {
	if conn == nil {
		log.Printf("[CONNECTION] [HEARTBEAT] action=heartbeat_check status=nil_connection")
		return false
	}

	clientAddr := conn.RemoteAddr().String()
	log.Printf("[CONNECTION] [HEARTBEAT] action=heartbeat_start client=%s timeout=2s", clientAddr)

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	err := conn.WriteJSON(map[string]interface{}{"type": "heartbeat"})
	conn.SetWriteDeadline(time.Time{})

	if err != nil {
		log.Printf("[CONNECTION] [HEARTBEAT] action=heartbeat_failed client=%s error=%v", clientAddr, err)
		return false
	}

	log.Printf("[CONNECTION] [HEARTBEAT] action=heartbeat_success client=%s", clientAddr)
	return true
}

func IsWebSocketConnectedSync(conn *websocket.Conn) bool {
	if conn == nil {
		log.Printf("[CONNECTION] [PING] action=ping_check status=nil_connection")
		return false
	}

	clientAddr := conn.RemoteAddr().String()
	log.Printf("[CONNECTION] [PING] action=ping_start client=%s timeout=100ms", clientAddr)

	conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	err := conn.WriteMessage(websocket.PingMessage, []byte{})
	conn.SetWriteDeadline(time.Time{})

	isConnected := err == nil
	if isConnected {
		log.Printf("[CONNECTION] [PING] action=ping_success client=%s", clientAddr)
	} else {
		log.Printf("[CONNECTION] [PING] action=ping_failed client=%s error=%v", clientAddr, err)
	}

	return isConnected
}

func addSocketToSpectators(room *OmokRoom, ws *websocket.Conn) {
	if room == nil || ws == nil {
		log.Printf("[SPECTATOR] [MANAGE] action=add_spectator status=invalid_params room_nil=%t ws_nil=%t", 
			(room == nil), (ws == nil))
		return
	}
	
	clientAddr := ws.RemoteAddr().String()
	log.Printf("[SPECTATOR] [MANAGE] action=add_spectator_start client=%s", clientAddr)
	
	room.spectatorsMux.Lock()
	defer room.spectatorsMux.Unlock()
	
	// Check if already exists
	for i, existingWs := range room.spectators {
		if existingWs == ws {
			log.Printf("[SPECTATOR] [MANAGE] action=add_spectator_duplicate client=%s existing_index=%d", 
				clientAddr, i)
			return
		}
	}
	
	room.spectators = append(room.spectators, ws)
	spectatorCount := len(room.spectators)
	log.Printf("[SPECTATOR] [MANAGE] action=add_spectator_success client=%s total_spectators=%d", 
		clientAddr, spectatorCount)
}

func removeSocketFromSpectators(room *OmokRoom, wsToRemove *websocket.Conn) {
	if room == nil || wsToRemove == nil {
		log.Printf("[SPECTATOR] [MANAGE] action=remove_spectator status=invalid_params room_nil=%t ws_nil=%t", 
			(room == nil), (wsToRemove == nil))
		return
	}
	
	clientAddr := wsToRemove.RemoteAddr().String()
	log.Printf("[SPECTATOR] [MANAGE] action=remove_spectator_start client=%s", clientAddr)
	
	room.spectatorsMux.Lock()
	defer room.spectatorsMux.Unlock()

	initialCount := len(room.spectators)
	newSpectators := []*websocket.Conn{}
	found := false
	
	for i, ws := range room.spectators {
		if ws != wsToRemove {
			newSpectators = append(newSpectators, ws)
		} else {
			found = true
			log.Printf("[SPECTATOR] [MANAGE] action=found_spectator_to_remove client=%s index=%d", 
				clientAddr, i)
		}
	}
	
	room.spectators = newSpectators
	finalCount := len(room.spectators)
	
	if found {
		log.Printf("[SPECTATOR] [MANAGE] action=remove_spectator_success client=%s removed=1 before=%d after=%d", 
			clientAddr, initialCount, finalCount)
	} else {
		log.Printf("[SPECTATOR] [MANAGE] action=remove_spectator_not_found client=%s spectator_count=%d", 
			clientAddr, finalCount)
	}
}

func removeWebSocketFromSocketsUnsafe(wsToRemove *websocket.Conn) {
	if wsToRemove == nil {
		log.Printf("[CONNECTION] [REMOVE] action=remove_socket status=nil_websocket")
		return
	}
	
	clientAddr := wsToRemove.RemoteAddr().String()
	initialCount := len(sockets)
	log.Printf("[CONNECTION] [REMOVE] action=remove_socket_start client=%s initial_count=%d", 
		clientAddr, initialCount)
	
	newSockets := []*websocket.Conn{}
	found := false
	
	for i, ws := range sockets {
		if ws != wsToRemove {
			newSockets = append(newSockets, ws)
		} else {
			found = true
			log.Printf("[CONNECTION] [REMOVE] action=found_socket_to_remove client=%s index=%d", 
				clientAddr, i)
		}
	}
	
	sockets = newSockets
	finalCount := len(sockets)
	
	if found {
		log.Printf("[CONNECTION] [REMOVE] action=remove_socket_success client=%s before=%d after=%d", 
			clientAddr, initialCount, finalCount)
	} else {
		log.Printf("[CONNECTION] [REMOVE] action=remove_socket_not_found client=%s socket_count=%d", 
			clientAddr, finalCount)
	}
}

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	clientAddr := r.RemoteAddr
	log.Printf("[HEALTH] [CHECK] action=health_check client=%s", clientAddr)
	
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	
	log.Printf("[HEALTH] [CHECK] action=health_response client=%s status=ok", clientAddr)
}

func cleanupDisconnectedWaitingPlayers() {
	initialRoomsCount := len(rooms)
	log.Printf("[CLEANUP] [WAITING_PLAYERS] action=cleanup_start rooms_count=%d", initialRoomsCount)
	
	newRooms := []*OmokRoom{}
	connectionsDecremented := 0
	roomsRemoved := 0

	for roomIndex, room := range rooms {
		keepRoom := true
		
		log.Printf("[CLEANUP] [WAITING_PLAYERS] action=checking_room room_index=%d user1_check=%t user2_check=%t user1_nick=%s", 
			roomIndex, room.user1.check, room.user2.check, room.user1.nickname)

		if room.user1.check && !room.user2.check {
			if room.user1.ws == nil {
				log.Printf("[CLEANUP] [WAITING_PLAYERS] action=removing_room_nil_ws room_index=%d user1=%s reason=nil_websocket", 
					roomIndex, room.user1.nickname)
				keepRoom = false
				roomsRemoved++
			} else {
				stillInGlobalList := false
				for _, socket := range sockets {
					if socket == room.user1.ws {
						stillInGlobalList = true
						break
					}
				}

				if !stillInGlobalList {
					log.Printf("[CLEANUP] [WAITING_PLAYERS] action=removing_room_not_in_list room_index=%d user1=%s reason=not_in_global_list", 
						roomIndex, room.user1.nickname)
					room.user1.ws.Close()
					keepRoom = false
					roomsRemoved++
				} else if !IsWebSocketConnectedSync(room.user1.ws) {
					log.Printf("[CLEANUP] [WAITING_PLAYERS] action=removing_room_unresponsive room_index=%d user1=%s reason=connection_failed", 
						roomIndex, room.user1.nickname)
					removeWebSocketFromSocketsUnsafe(room.user1.ws)
					room.user1.ws.Close()
					connectionsCount--
					connectionsDecremented++
					keepRoom = false
					roomsRemoved++
				} else {
					log.Printf("[CLEANUP] [WAITING_PLAYERS] action=keeping_room room_index=%d user1=%s status=responsive", 
						roomIndex, room.user1.nickname)
				}
			}
		} else {
			log.Printf("[CLEANUP] [WAITING_PLAYERS] action=keeping_room room_index=%d reason=not_waiting_for_player2", roomIndex)
		}

		if keepRoom {
			newRooms = append(newRooms, room)
		}
	}

	rooms = newRooms
	finalRoomsCount := len(rooms)

	log.Printf("[CLEANUP] [WAITING_PLAYERS] action=cleanup_complete rooms_before=%d rooms_after=%d rooms_removed=%d connections_decremented=%d", 
		initialRoomsCount, finalRoomsCount, roomsRemoved, connectionsDecremented)

	if connectionsDecremented > 0 {
		log.Printf("[CLEANUP] [WAITING_PLAYERS] action=broadcasting_new_count current_connections=%d", connectionsCount)
		BroadcastConnectionsCountUnsafe(connectionsCount)
	}
}
func backgroundCleanup() {
	log.Printf("[BACKGROUND] [CLEANUP] action=background_cleanup_start interval=15s")
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	cleanupCycle := 0
	for range ticker.C {
		cleanupCycle++
		log.Printf("[BACKGROUND] [CLEANUP] action=cleanup_cycle_start cycle=%d", cleanupCycle)
		
		globalMutex.Lock()

		initialConnectionCount := connectionsCount
		actualSocketCount := len(sockets)
		
		log.Printf("[BACKGROUND] [CLEANUP] action=checking_connection_count cycle=%d stored=%d actual=%d", 
			cleanupCycle, initialConnectionCount, actualSocketCount)
			
		if connectionsCount != actualSocketCount {
			log.Printf("[BACKGROUND] [CLEANUP] action=connection_count_mismatch cycle=%d stored=%d actual=%d status=correcting",
				cleanupCycle, connectionsCount, actualSocketCount)
			connectionsCount = actualSocketCount
			BroadcastConnectionsCountUnsafe(connectionsCount)
		}

		initialRoomCount := len(rooms)
		log.Printf("[BACKGROUND] [CLEANUP] action=starting_room_cleanup cycle=%d initial_rooms=%d", 
			cleanupCycle, initialRoomCount)
		cleanupDisconnectedWaitingPlayers()

		log.Printf("[BACKGROUND] [CLEANUP] action=checking_socket_health cycle=%d total_sockets=%d", 
			cleanupCycle, len(sockets))
		validSockets := []*websocket.Conn{}
		deadSockets := 0
		
		for i, socket := range sockets {
			if socket != nil && IsWebSocketConnectedSync(socket) {
				validSockets = append(validSockets, socket)
			} else {
				deadSockets++
				log.Printf("[BACKGROUND] [CLEANUP] action=found_dead_socket cycle=%d socket_index=%d socket_nil=%t", 
					cleanupCycle, i, (socket == nil))
				if socket != nil {
					socket.Close()
				}
			}
		}

		if len(validSockets) != len(sockets) {
			removedCount := len(sockets) - len(validSockets)
			sockets = validSockets
			connectionsCount = len(sockets)
			log.Printf("[BACKGROUND] [CLEANUP] action=removed_dead_sockets cycle=%d removed=%d new_count=%d", 
				cleanupCycle, removedCount, connectionsCount)
		}

		finalRoomCount := len(rooms)
		finalConnectionCount := connectionsCount

		if initialRoomCount != finalRoomCount || initialConnectionCount != finalConnectionCount {
			log.Printf("[BACKGROUND] [CLEANUP] action=state_changed cycle=%d rooms=%d->%d connections=%d->%d status=broadcasting",
				cleanupCycle, initialRoomCount, finalRoomCount, initialConnectionCount, finalConnectionCount)
			BroadcastConnectionsCountUnsafe(connectionsCount)
		} else {
			log.Printf("[BACKGROUND] [CLEANUP] action=no_changes cycle=%d rooms=%d connections=%d", 
				cleanupCycle, finalRoomCount, finalConnectionCount)
		}

		globalMutex.Unlock()
		log.Printf("[BACKGROUND] [CLEANUP] action=cleanup_cycle_complete cycle=%d", cleanupCycle)
	}
}

func main() {
	log.Printf("[SERVER] [STARTUP] action=server_init address=%s", serverAddress)
	
	log.Printf("[SERVER] [STARTUP] action=starting_background_cleanup")
	go backgroundCleanup()

	log.Printf("[SERVER] [STARTUP] action=registering_handlers")
	http.HandleFunc("/health", HealthHandler)
	log.Printf("[SERVER] [STARTUP] action=registered_handler endpoint=/health")
	
	http.HandleFunc("/game", SocketHandler)
	log.Printf("[SERVER] [STARTUP] action=registered_handler endpoint=/game")
	
	http.HandleFunc("/spectator", SpectatorHandler)
	log.Printf("[SERVER] [STARTUP] action=registered_handler endpoint=/spectator")

	log.Printf("[SERVER] [STARTUP] action=server_starting address=%s status=listening", serverAddress)
	if err := http.ListenAndServe(serverAddress, nil); err != nil {
		log.Fatalf("[SERVER] [STARTUP] action=server_failed address=%s error=%v", serverAddress, err)
	}
}
