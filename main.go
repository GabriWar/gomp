package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	WORLD_WIDTH    = 150
	WORLD_HEIGHT   = 40
	BULLET_SPEED   = 100 * time.Millisecond
	SHOOT_COOLDOWN = 500 * time.Millisecond
	RESPAWN_TIME   = 3 * time.Second
)

type Player struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	X           int       `json:"x"`
	Y           int       `json:"y"`
	Character   string    `json:"character"`
	Kills       int       `json:"kills"`
	Deaths      int       `json:"deaths"`
	LastSeen    time.Time `json:"lastSeen"`
	Dead        bool      `json:"dead"`
	RespawnAt   time.Time `json:"respawnAt"`
	LastShot    time.Time `json:"lastShot"`
	IsSpectator bool      `json:"isSpectator"`
}

type Bullet struct {
	ID        string
	X         int
	Y         int
	DirX      int
	DirY      int
	OwnerID   string
	Character string
}

type Message struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type MoveData struct {
	Direction string `json:"direction"`
}

type ShootData struct {
	Direction string `json:"direction"`
}

type JoinData struct {
	Name      string `json:"name"`
	Character string `json:"character"`
	Spectator bool   `json:"spectator"`
}

type GameWorld struct {
	Width   int
	Height  int
	Grid    [][]string
	Bullets map[string]*Bullet
}

type GameServer struct {
	clients map[*websocket.Conn]*clientInfo
	players map[string]*Player
	world   *GameWorld
	mutex   sync.RWMutex
}

type clientInfo struct {
	player *Player
	mu     sync.Mutex
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	gameServer = &GameServer{
		clients: make(map[*websocket.Conn]*clientInfo),
		players: make(map[string]*Player),
		world:   NewGameWorld(),
	}
)

func NewGameWorld() *GameWorld {
	world := &GameWorld{
		Width:   WORLD_WIDTH,
		Height:  WORLD_HEIGHT,
		Grid:    make([][]string, WORLD_HEIGHT),
		Bullets: make(map[string]*Bullet),
	}

	for i := range world.Grid {
		world.Grid[i] = make([]string, WORLD_WIDTH)
	}

	return world
}

func (gw *GameWorld) Render(players map[string]*Player) string {
	for y := 0; y < gw.Height; y++ {
		for x := 0; x < gw.Width; x++ {
			gw.Grid[y][x] = " "
		}
	}

	for _, bullet := range gw.Bullets {
		if bullet.X >= 0 && bullet.X < gw.Width && bullet.Y >= 0 && bullet.Y < gw.Height {
			gw.Grid[bullet.Y][bullet.X] = "*"
		}
	}

	for _, player := range players {
		if !player.Dead && player.X >= 0 && player.X < gw.Width && player.Y >= 0 && player.Y < gw.Height {
			gw.Grid[player.Y][player.X] = player.Character
		}
	}

	var builder strings.Builder
	builder.WriteString("+" + strings.Repeat("-", gw.Width) + "+\n")

	for y := 0; y < gw.Height; y++ {
		builder.WriteString("|")
		for x := 0; x < gw.Width; x++ {
			builder.WriteString(gw.Grid[y][x])
		}
		builder.WriteString("|\n")
	}

	builder.WriteString("+" + strings.Repeat("-", gw.Width) + "+\n")

	return builder.String()
}

func (gs *GameServer) addClient(conn *websocket.Conn, player *Player) {
	gs.mutex.Lock()
	gs.clients[conn] = &clientInfo{player: player}
	gs.players[player.ID] = player

	worldSnapshot := gs.world.Render(gs.players)

	playersSnapshot := make([]map[string]interface{}, 0, len(gs.players))
	playersForLeaderboard := make([]*Player, 0, len(gs.players))

	for _, p := range gs.players {
		status := "Alive"
		if p.Dead {
			status = fmt.Sprintf("Dead (%.1fs)", time.Until(p.RespawnAt).Seconds())
		}

		playersSnapshot = append(playersSnapshot, map[string]interface{}{
			"id":        p.ID,
			"name":      p.Name,
			"character": p.Character,
			"position":  fmt.Sprintf("(%d,%d)", p.X, p.Y),
			"kills":     p.Kills,
			"deaths":    p.Deaths,
			"status":    status,
		})

		playersForLeaderboard = append(playersForLeaderboard, p)
	}

	gs.mutex.Unlock()

	leaderboardSnapshot := make([]map[string]interface{}, 0, len(playersForLeaderboard))
	sort.Slice(playersForLeaderboard, func(i, j int) bool {
		if playersForLeaderboard[i].Kills == playersForLeaderboard[j].Kills {
			return playersForLeaderboard[i].Deaths < playersForLeaderboard[j].Deaths
		}
		return playersForLeaderboard[i].Kills > playersForLeaderboard[j].Kills
	})

	for i, p := range playersForLeaderboard {
		kdr := float64(p.Kills)
		if p.Deaths > 0 {
			kdr = float64(p.Kills) / float64(p.Deaths)
		}

		leaderboardSnapshot = append(leaderboardSnapshot, map[string]interface{}{
			"rank":      i + 1,
			"name":      p.Name,
			"character": p.Character,
			"kills":     p.Kills,
			"deaths":    p.Deaths,
			"kdr":       fmt.Sprintf("%.2f", kdr),
		})
	}

	gs.sendToClient(conn, Message{
		Type: "welcome",
		Data: map[string]interface{}{
			"playerId":    player.ID,
			"world":       worldSnapshot,
			"players":     playersSnapshot,
			"leaderboard": leaderboardSnapshot,
		},
	})

	gs.broadcastWorldUpdate()
	gs.broadcastPlayerList()
	gs.broadcastLeaderboard()
}

func (gs *GameServer) removeClient(conn *websocket.Conn) {
	gs.mutex.Lock()
	var shouldBroadcast bool
	if ci, exists := gs.clients[conn]; exists {
		delete(gs.clients, conn)
		if ci.player != nil {
			delete(gs.players, ci.player.ID)
		}
		shouldBroadcast = true
	}
	gs.mutex.Unlock()

	if shouldBroadcast {
		gs.broadcastWorldUpdate()
		gs.broadcastPlayerList()
		gs.broadcastLeaderboard()
	}
}

func (gs *GameServer) movePlayer(playerID, direction string) bool {
	gs.mutex.Lock()
	player, exists := gs.players[playerID]
	if !exists || player.Dead || player.IsSpectator {
		gs.mutex.Unlock()
		return false
	}

	newX, newY := player.X, player.Y

	switch direction {
	case "up":
		newY = int(math.Max(0, float64(player.Y-1)))
	case "down":
		newY = int(math.Min(float64(WORLD_HEIGHT-1), float64(player.Y+1)))
	case "left":
		newX = int(math.Max(0, float64(player.X-1)))
	case "right":
		newX = int(math.Min(float64(WORLD_WIDTH-1), float64(player.X+1)))
	default:
		gs.mutex.Unlock()
		return false
	}

	for _, p := range gs.players {
		if p.ID != playerID && !p.Dead && p.X == newX && p.Y == newY {
			gs.mutex.Unlock()
			return false
		}
	}

	player.X = newX
	player.Y = newY
	player.LastSeen = time.Now()
	gs.mutex.Unlock()

	gs.broadcastWorldUpdate()
	return true
}

func (gs *GameServer) shootBullet(playerID, direction string) bool {
	gs.mutex.Lock()
	defer gs.mutex.Unlock()

	player, exists := gs.players[playerID]
	if !exists || player.Dead || player.IsSpectator {
		return false
	}

	if time.Since(player.LastShot) < SHOOT_COOLDOWN {
		return false
	}

	dirX, dirY := 0, 0
	switch direction {
	case "up":
		dirY = -1
	case "down":
		dirY = 1
	case "left":
		dirX = -1
	case "right":
		dirX = 1
	default:
		return false
	}

	bullet := &Bullet{
		ID:        fmt.Sprintf("bullet_%d", time.Now().UnixNano()),
		X:         player.X,
		Y:         player.Y,
		DirX:      dirX,
		DirY:      dirY,
		OwnerID:   playerID,
		Character: "*",
	}

	gs.world.Bullets[bullet.ID] = bullet

	go gs.moveBullet(bullet.ID)

	player.LastShot = time.Now()

	return true
}

func (gs *GameServer) moveBullet(bulletID string) {
	for {
		time.Sleep(BULLET_SPEED)

		gs.mutex.Lock()
		bullet, exists := gs.world.Bullets[bulletID]
		if !exists {
			gs.mutex.Unlock()
			return
		}

		bullet.X += bullet.DirX
		bullet.Y += bullet.DirY

		if bullet.X < 0 || bullet.X >= WORLD_WIDTH || bullet.Y < 0 || bullet.Y >= WORLD_HEIGHT {
			delete(gs.world.Bullets, bulletID)
			gs.mutex.Unlock()
			gs.broadcastWorldUpdate()
			return
		}

		for _, player := range gs.players {
			if !player.Dead && player.X == bullet.X && player.Y == bullet.Y && player.ID != bullet.OwnerID {
				player.Dead = true
				player.Deaths++
				player.RespawnAt = time.Now().Add(RESPAWN_TIME)

				if shooter, exists := gs.players[bullet.OwnerID]; exists {
					shooter.Kills++
				}

				delete(gs.world.Bullets, bulletID)

				go gs.respawnPlayer(player.ID)

				gs.mutex.Unlock()
				gs.broadcastWorldUpdate()
				gs.broadcastPlayerList()
				gs.broadcastLeaderboard()
				return
			}
		}

		gs.mutex.Unlock()
		gs.broadcastWorldUpdate()
	}
}

func (gs *GameServer) respawnPlayer(playerID string) {
	time.Sleep(RESPAWN_TIME)

	gs.mutex.Lock()
	player, exists := gs.players[playerID]
	if !exists {
		gs.mutex.Unlock()
		return
	}

	for attempts := 0; attempts < 50; attempts++ {
		x := int(time.Now().UnixNano() % int64(WORLD_WIDTH))
		y := int(time.Now().UnixNano() % int64(WORLD_HEIGHT))

		occupied := false
		for _, p := range gs.players {
			if !p.Dead && p.X == x && p.Y == y {
				occupied = true
				break
			}
		}

		if !occupied {
			player.X = x
			player.Y = y
			player.Dead = false
			break
		}
	}

	if player.Dead {
		player.X = WORLD_WIDTH / 2
		player.Y = WORLD_HEIGHT / 2
		player.Dead = false
	}

	gs.mutex.Unlock()

	gs.broadcastWorldUpdate()
	gs.broadcastPlayerList()
}

func (gs *GameServer) getPlayerList() []map[string]interface{} {
	gs.mutex.RLock()
	defer gs.mutex.RUnlock()

	var playerList []map[string]interface{}
	for _, player := range gs.players {
		status := "Alive"
		if player.Dead {
			status = fmt.Sprintf("Dead (%.1fs)", time.Until(player.RespawnAt).Seconds())
		}

		playerList = append(playerList, map[string]interface{}{
			"id":        player.ID,
			"name":      player.Name,
			"character": player.Character,
			"position":  fmt.Sprintf("(%d,%d)", player.X, player.Y),
			"kills":     player.Kills,
			"deaths":    player.Deaths,
			"status":    status,
		})
	}

	return playerList
}

func (gs *GameServer) getLeaderboard() []map[string]interface{} {
	gs.mutex.RLock()
	playersSnapshot := make([]*Player, 0, len(gs.players))
	for _, player := range gs.players {
		playersSnapshot = append(playersSnapshot, player)
	}
	gs.mutex.RUnlock()

	var leaderboard []map[string]interface{}

	sort.Slice(playersSnapshot, func(i, j int) bool {
		if playersSnapshot[i].Kills == playersSnapshot[j].Kills {
			return playersSnapshot[i].Deaths < playersSnapshot[j].Deaths
		}
		return playersSnapshot[i].Kills > playersSnapshot[j].Kills
	})

	for i, player := range playersSnapshot {
		kdr := float64(player.Kills)
		if player.Deaths > 0 {
			kdr = float64(player.Kills) / float64(player.Deaths)
		}

		leaderboard = append(leaderboard, map[string]interface{}{
			"rank":      i + 1,
			"name":      player.Name,
			"character": player.Character,
			"kills":     player.Kills,
			"deaths":    player.Deaths,
			"kdr":       fmt.Sprintf("%.2f", kdr),
		})
	}

	return leaderboard
}

func (gs *GameServer) sendToClient(conn *websocket.Conn, msg Message) {
	gs.mutex.RLock()
	ci, exists := gs.clients[conn]
	gs.mutex.RUnlock()
	if !exists {
		return
	}

	ci.mu.Lock()
	defer ci.mu.Unlock()

	if err := conn.WriteJSON(msg); err != nil {
		log.Printf("Error sending message to client: %v", err)
		conn.Close()
		gs.removeClient(conn)
	}
}

func (gs *GameServer) broadcast(msg Message) {
	gs.mutex.RLock()
	conns := make([]*websocket.Conn, 0, len(gs.clients))
	for conn := range gs.clients {
		conns = append(conns, conn)
	}
	gs.mutex.RUnlock()

	for _, conn := range conns {
		gs.mutex.RLock()
		ci, exists := gs.clients[conn]
		gs.mutex.RUnlock()
		if !exists {
			continue
		}

		ci.mu.Lock()
		err := conn.WriteJSON(msg)
		ci.mu.Unlock()
		if err != nil {
			log.Printf("Error broadcasting message: %v", err)
			conn.Close()
			gs.removeClient(conn)
		}
	}
}

func (gs *GameServer) broadcastWorldUpdate() {
	gs.mutex.RLock()
	worldStr := gs.world.Render(gs.players)
	gs.mutex.RUnlock()

	gs.broadcast(Message{
		Type: "worldUpdate",
		Data: worldStr,
	})
}

func (gs *GameServer) broadcastPlayerList() {
	gs.broadcast(Message{
		Type: "playerList",
		Data: gs.getPlayerList(),
	})
}

func (gs *GameServer) broadcastLeaderboard() {
	gs.broadcast(Message{
		Type: "leaderboard",
		Data: gs.getLeaderboard(),
	})
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	var player *Player

	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		switch msg.Type {
		case "join":
			data, _ := json.Marshal(msg.Data)
			var joinData JoinData
			json.Unmarshal(data, &joinData)

			if len(joinData.Character) != 1 {
				continue
			}

			player = &Player{
				ID:          fmt.Sprintf("p%d", time.Now().UnixNano()%10000),
				Name:        joinData.Name,
				X:           WORLD_WIDTH / 2,
				Y:           WORLD_HEIGHT / 2,
				Character:   joinData.Character,
				Kills:       0,
				Deaths:      0,
				Dead:        false,
				LastSeen:    time.Now(),
				IsSpectator: joinData.Spectator,
			}

			gameServer.addClient(conn, player)
			log.Printf("Player %s (%s) joined the game", player.Name, player.Character)

		case "move":
			if player != nil {
				data, _ := json.Marshal(msg.Data)
				var moveData MoveData
				json.Unmarshal(data, &moveData)

				if gameServer.movePlayer(player.ID, moveData.Direction) {
					log.Printf("Player %s moved %s to (%d,%d)", player.Name, moveData.Direction, player.X, player.Y)
				}
			}

		case "shoot":
			if player != nil {
				data, _ := json.Marshal(msg.Data)
				var shootData ShootData
				json.Unmarshal(data, &shootData)

				if gameServer.shootBullet(player.ID, shootData.Direction) {
					log.Printf("Player %s shot %s", player.Name, shootData.Direction)
				}
			}
		}
	}

	if player != nil {
		gameServer.removeClient(conn)
		log.Printf("Player %s left the game", player.Name)
	}
}

func serveHTML(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html>
<head>
	<title>ARENA DE BATALHA ASCII</title>
    <style>
		body {
			margin: 0;
			padding: 0;
			font-family: 'Courier New', monospace;
			background: #000000;
			color: #ffffff;
		}
        .container {
            max-width: 1200px;
            margin: 0 auto;
        }
        h1 {
            color: #ff0000;
            text-align: center;
            text-shadow: 0 0 10px #ff0000;
        }
        #joinForm {
            text-align: center;
            margin: 20px 0;
            padding: 20px;
            border: 1px solid #00ff00;
            border-radius: 5px;
        }
        #joinForm input {
            background: #2a2a2a;
            border: 1px solid #00ff00;
            color: #00ff00;
            padding: 10px;
            margin: 5px;
            font-family: 'Courier New', monospace;
            font-size: 16px;
        }
        #joinForm button {
            background: #003300;
            border: 1px solid #00ff00;
            color: #00ff00;
            padding: 10px 20px;
            font-family: 'Courier New', monospace;
            font-size: 16px;
            cursor: pointer;
        }
        #joinForm button:hover {
            background: #00ff00;
            color: #000000;
        }
        #gameArea {
            display: flex;
            gap: 20px;
        }
		#worldDisplay {
			flex: 2;
			background: #000000;
			color: #ffffff;
			padding: 10px;
			border: 1px solid rgba(255,255,255,0.06);
			border-radius: 5px;
			overflow: hidden;
		}
		#worldDisplay pre {
			margin: 0;
			font-size: 12px; 
			line-height: 1.1;
			color: #ffffff;
			white-space: pre;
			background: transparent;
			display: block;
			margin: 0 auto;
			max-width: calc(100vw - 40px);
			max-height: calc(100vh - 40px);
			overflow: hidden;
		}

		#worldDisplay pre .bullet {
			color: #ff0000;
		}

		#worldDisplay pre .me {
			color: #00aa00;
			font-weight: bold;
		}
        #gameInfo {
            flex: 1;
            display: flex;
            flex-direction: column;
            gap: 10px;
        }
		.info-panel {
			background: rgba(0,0,0,0.6);
			color: #ffffff;
			padding: 10px;
			border: 1px solid rgba(255,255,255,0.06);
			border-radius: 5px;
			max-height: 200px;
			overflow-y: auto;
		}
		#controls {
			text-align: center;
			padding: 10px;
		}
		.fullscreen-world #worldDisplay {
			position: fixed;
			top: 0;
			left: 0;
			width: 100vw;
			height: 100vh;
			padding: 6px;
			box-sizing: border-box;
			z-index: 9999;
			overflow: auto;
			display: flex;
			align-items: center;
			justify-content: center;
		}

		.fullscreen-world.spectator #worldDisplay {
			display: none !important;
		}

		.fullscreen-world #gameInfo {
			position: fixed;
			top: 12px;
			right: 12px;
			width: 340px;
			max-height: calc(100vh - 24px);
			z-index: 10001;
			background: transparent;
			border: 1px solid rgba(255,255,255,0.06);
			border-radius: 6px;
			padding: 8px;
			overflow: auto;
			box-shadow: none;
		}

		.fullscreen-world #gameInfo .info-panel {
			background: transparent;
			border: none;
			padding: 4px 6px;
		}

		.fullscreen-world.spectator #controls {
			display: none;
		}

		.fullscreen-world:not(.spectator) #controls {
			display: block;
		}
		.control-btn {
			background: #003300;
			border: 1px solid #00ff00;
			color: #00ff00;
			padding: 0;
			margin: 2px;
			font-family: 'Courier New', monospace;
			cursor: pointer;
			font-size: 14px;
			width: 36px;
			height: 36px;
			display: inline-flex;
			align-items: center;
			justify-content: center;
		}
        .control-btn:hover {
            background: #00ff00;
            color: #000000;
        }
		.shoot-btn {
			background: #330000;
			border: 1px solid #ff0000;
			color: #ff0000;
			padding: 0;
			width: 36px;
			height: 36px;
			display: inline-flex;
			align-items: center;
			justify-content: center;
			font-size: 14px;
		}
        .shoot-btn:hover {
            background: #ff0000;
            color: #000000;
        }
        .control-row {
            margin: 5px 0;
        }
        .player-item, .leaderboard-item {
            padding: 3px;
            font-size: 11px;
        }
        .hidden {
            display: none;
        }
        .instructions {
            margin: 10px 0;
            padding: 10px;
            border: 1px solid #333;
            border-radius: 5px;
            background: #252525;
            font-size: 12px;
        }
        h3 {
            margin: 0 0 10px 0;
            color: #00ff00;
        }
    </style>
</head>
<body>
    <div class="container">
		<div id="joinForm">
			<h2>Entrar</h2>
			<div>
				<input type="text" id="playerName" placeholder="Nome do jogador" maxlength="15">
			</div>
			<div>
				<input type="text" id="playerCharacter" placeholder="Seu caractere (A-Z, 0-9, @#$%&)" maxlength="1">
			</div>
			<div>
				<label><input type="checkbox" id="spectatorCheckbox"> Entrar como espectador</label>
			</div>
			<div>
				<button onclick="joinGame()">ENTRAR</button>
			</div>
			 <div id="controls">
                        <div class="control-row">
                            <button class="shoot-btn" onclick="shoot('up')">I</button>
                        </div>
						<div class="control-row">
							<button class="control-btn" onclick="move('up')">&#9650;</button>
						</div>
                        <div class="control-row">
                            <button class="shoot-btn" onclick="shoot('left')">J</button>
							<button class="control-btn" onclick="move('left')">&#9664;</button>
							<button class="control-btn" onclick="move('down')">&#9660;</button>
							<button class="control-btn" onclick="move('right')">&#9654;</button>
                            <button class="shoot-btn" onclick="shoot('right')">L</button>
                        </div>
                        <div class="control-row">
                            <button class="shoot-btn" onclick="shoot('down')">K</button>
                        </div>
                    </div>
                </div>
		</div>
        
        <div id="gameArea" class="hidden">
			<div id="worldDisplay" class="hidden">
				<pre id="world"></pre>
			</div>
            
            <div id="gameInfo">
                <div class="info-panel">
                    <h3>PLACAR:</h3>
                    <div id="leaderboard"></div>
                </div>
                
                <div class="info-panel">
					<h3>JOGADORES ONLINE:</h3>
                    <div id="players"></div>
                </div>
            </div>
        </div>
    </div>

    <script>
        let socket;
        let myPlayerId = null;

		function joinGame() {
			const name = document.getElementById('playerName').value.trim();
			const character = document.getElementById('playerCharacter').value.trim();
			const spectator = document.getElementById('spectatorCheckbox').checked;

			if (!name) {
				alert('Por favor, digite seu nome!');
				return;
			}

			if (!spectator && (!character || character.length !== 1)) {
				alert('Por favor, digite exatamente um caractere!');
				return;
			}

			document.getElementById('joinForm').classList.add('hidden');
			document.getElementById('gameArea').classList.remove('hidden');

			if (spectator) {
				document.getElementById('worldDisplay').classList.add('hidden');
			} else {
				document.getElementById('worldDisplay').classList.remove('hidden');
			}

			document.body.classList.add('fullscreen-world');
			if (spectator) {
				document.body.classList.add('spectator');
			} else {
				document.body.classList.remove('spectator');
			}

			const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
			socket = new WebSocket(protocol + '//' + window.location.host + '/ws');

			socket.onopen = function() {
				socket.send(JSON.stringify({
					type: 'join',
					data: {
						name: name,
						character: character,
						spectator: spectator
					}
				}));
			};

			socket.onmessage = function(event) {
				const msg = JSON.parse(event.data);
				handleMessage(msg);
			};

			socket.onclose = function() {
				console.log('Connection closed');
				alert('Conexão perdida! Por favor, atualize a página.');
			};
		}

        function handleMessage(msg) {
            switch (msg.type) {
                case 'welcome':
                    myPlayerId = msg.data.playerId;
					renderWorld(msg.data.world);
                    updatePlayerList(msg.data.players);
                    updateLeaderboard(msg.data.leaderboard);
                    break;
                    
                case 'worldUpdate':
					renderWorld(msg.data);
                    break;
                    
                case 'playerList':
                    updatePlayerList(msg.data);
                    break;
                    
                case 'leaderboard':
                    updateLeaderboard(msg.data);
                    break;
            }
        }

		function renderWorld(worldText) {
			if (!myPlayerId) {
				document.getElementById('world').textContent = worldText;
				return;
			}

			const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
			let myChar = null;
			const playersDiv = document.getElementById('players');
			const worldEsc = esc(worldText);

			let html = worldEsc.replace(/\*/g, '<span class="bullet">*</span>');

			const playerItems = Array.from(document.querySelectorAll('#players .player-item'));
			for (const item of playerItems) {
				if (item.textContent && item.textContent.includes('(you)')) {
					
				}
			}

			document.getElementById('world').innerHTML = html;
		}

		document.addEventListener('DOMContentLoaded', function() {
			const spectatorCheckbox = document.getElementById('spectatorCheckbox');
			const charInput = document.getElementById('playerCharacter');
			if (spectatorCheckbox) {
				spectatorCheckbox.addEventListener('change', function() {
					if (spectatorCheckbox.checked) {
						charInput.disabled = true;
						charInput.value = '';
					} else {
						charInput.disabled = false;
					}
				});
			}
		});
		
		function updatePlayerList(players) {
			const playersDiv = document.getElementById('players');
			playersDiv.innerHTML = '';

			players.forEach(player => {
				const playerDiv = document.createElement('div');
				playerDiv.className = 'player-item';
				playerDiv.innerHTML = player.character + ' - ' + player.name + ' (' + player.kills + '/' + player.deaths + ') ' + player.status;
				playersDiv.appendChild(playerDiv);
			});
		}

        function updateLeaderboard(leaderboard) {
            const leaderboardDiv = document.getElementById('leaderboard');
            leaderboardDiv.innerHTML = '';
            
			leaderboard.forEach(player => {
				const playerDiv = document.createElement('div');
				playerDiv.className = 'leaderboard-item';
				playerDiv.innerHTML = player.rank + '. ' + player.character + ' ' + player.name + ' - ' + player.kills + 'K/' + player.deaths + 'D (KDR: ' + player.kdr + ')';
				leaderboardDiv.appendChild(playerDiv);
			});
        }

        function move(direction) {
            if (socket && socket.readyState === WebSocket.OPEN) {
                socket.send(JSON.stringify({
                    type: 'move',
                    data: {
                        direction: direction
                    }
                }));
            }
        }

        function shoot(direction) {
            if (socket && socket.readyState === WebSocket.OPEN) {
                socket.send(JSON.stringify({
                    type: 'shoot',
                    data: {
                        direction: direction
                    }
                }));
            }
        }

        document.addEventListener('keydown', function(event) {
            if (myPlayerId) {
                switch(event.key.toLowerCase()) {
                    case 'w':
                    case 'arrowup':
                        move('up');
                        event.preventDefault();
                        break;
                    case 's':
                    case 'arrowdown':
                        move('down');
                        event.preventDefault();
                        break;
                    case 'a':
                    case 'arrowleft':
                        move('left');
                        event.preventDefault();
                        break;
                    case 'd':
                    case 'arrowright':
                        move('right');
                        event.preventDefault();
                        break;
                    case 'i':
                        shoot('up');
                        event.preventDefault();
                        break;
                    case 'k':
                        shoot('down');
                        event.preventDefault();
                        break;
                    case 'j':
                        shoot('left');
                        event.preventDefault();
                        break;
                    case 'l':
                        shoot('right');
                        event.preventDefault();
                        break;
                }
            }
        });
    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, html)
}

func main() {
	http.HandleFunc("/", serveHTML)
	http.HandleFunc("/ws", handleWebSocket)

	port := ":3000"
	fmt.Printf("Iniciando servidor ARENA DE BATALHA ASCII em http://localhost%s\n", port)
	fmt.Println("Jogadores podem mover, atirar, eliminar e competir pelo maior placar!")

	log.Fatal(http.ListenAndServe(port, nil))
}
