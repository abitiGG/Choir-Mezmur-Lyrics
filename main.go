package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var adminIDs = []int{547900737, 1237680623} // Admin Telegram ID

type UserState struct {
	Stage     string
	Title     string
	Lyrics    string
	Category  string
	IsEditing bool
	EditField string
}

var userStates = make(map[int]UserState)

func main() {
	// Load environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	// Retrieve environment variables
	telegramBotToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	mongoDBURI := os.Getenv("MONGODB_URI")

	// Connect to MongoDB
	client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(mongoDBURI))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(context.TODO())

	collection := client.Database("lyrics_bot").Collection("lyrics")

	if telegramBotToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is not set")
	}

	bot, err := tgbotapi.NewBotAPI(telegramBotToken)
	if err != nil {
		log.Panicf("Failed to create bot: %v", err)
	}

	bot.Debug = true
	fmt.Printf("Authorized on account %s\n", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Panicf("Failed to get updates channel: %v", err)
	}

	for update := range updates {
		if update.Message != nil {
			handleUpdate(bot, update, collection)
		} else if update.CallbackQuery != nil {
			handleCallbackQuery(bot, update.CallbackQuery, collection)
		}
	}
}

func handleUpdate(bot *tgbotapi.BotAPI, update tgbotapi.Update, collection *mongo.Collection) {
	if update.Message == nil {
		return
	}

	// Handle commands
	if update.Message.IsCommand() {
		switch update.Message.Command() {
		case "start":
			sendMainMenu(bot, update.Message.Chat.ID)
		case "help":
			helpCommand(bot, update.Message)
		case "lyrics":
			lyricsCommand(bot, update.Message, collection)
		case "addsong":
			if isAdmin(update.Message.From.ID) {
				addSongCommand(bot, update.Message, collection)
			} else {
				bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "You are not authorized to add songs."))
			}
		case "uploadimage":
			if isAdmin(update.Message.From.ID) {
				uploadImageCommand(bot, update.Message)
			} else {
				bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "You are not authorized to upload images."))
			}
		case "cancel":
			if _, exists := userStates[update.Message.From.ID]; exists {
				delete(userStates, update.Message.From.ID)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"Song addition cancelled.")
				bot.Send(msg)
			}
		default:
			defaultMessage(bot, update.Message, collection)
		}
		return
	}

	// Handle button presses
	switch update.Message.Text {
	case "🎵 Search Lyrics":
		msg := tgbotapi.NewMessage(update.Message.Chat.ID,
			"Please enter a letter (A-Z) to see available songs, or use /lyrics <song title> to search directly.")
		bot.Send(msg)

	case "📝 View All Songs":
		msg := tgbotapi.NewMessage(update.Message.Chat.ID,
			"Please select a letter (A-Z) to see songs starting with that letter:")
		bot.Send(msg)

	case "⬆️ Upload Image":
		if isAdmin(update.Message.From.ID) {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"Please send me the image you want to upload.")
			bot.Send(msg)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"You are not authorized to upload images.")
			bot.Send(msg)
		}

	case "➕ Add Song":
		if isAdmin(update.Message.From.ID) {
			userStates[update.Message.From.ID] = UserState{Stage: "awaiting_title"}
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"Please enter the song title:\n(or type /cancel to abort)")
			bot.Send(msg)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"You are not authorized to add songs.")
			bot.Send(msg)
		}

	case "❓ Help":
		helpText := "Here's how to use the bot:\n\n" +
			"🎵 Search Lyrics - Search for song lyrics\n" +
			"📝 View All Songs - Browse songs by letter\n" +
			"⬆️ Upload Image - Upload song images (Admin only)\n" +
			"➕ Add Song - Add new songs (Admin only)\n\n" +
			"Commands:\n" +
			"/lyrics <song title> - Get lyrics for a specific song\n" +
			"/start - Show main menu\n" +
			"/help - Show this help message"

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, helpText)
		bot.Send(msg)

	case "👥 Choir Songs":
		showSongsByCategory(bot, update.Message, collection, "Choir")

	case "🎵 Non-Choir Songs":
		showSongsByCategory(bot, update.Message, collection, "Non-Choir")

	case "✏️ Edit Song":
		if isAdmin(update.Message.From.ID) {
			userStates[update.Message.From.ID] = UserState{
				Stage:     "edit_select_song",
				IsEditing: true,
			}
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"Please enter the title of the song you want to edit:")
			bot.Send(msg)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID,
				"You are not authorized to edit songs.")
			bot.Send(msg)
		}

	default:
		if isAdmin(update.Message.From.ID) {
			if state, exists := userStates[update.Message.From.ID]; exists {
				switch state.Stage {
				case "awaiting_title":
					userStates[update.Message.From.ID] = UserState{
						Stage: "awaiting_category",
						Title: update.Message.Text,
					}
					keyboard := tgbotapi.NewReplyKeyboard(
						tgbotapi.NewKeyboardButtonRow(
							tgbotapi.NewKeyboardButton("Choir"),
							tgbotapi.NewKeyboardButton("Non-Choir"),
						),
					)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"Please select the song category:")
					msg.ReplyMarkup = keyboard
					bot.Send(msg)
					return

				case "awaiting_category":
					if update.Message.Text != "Choir" && update.Message.Text != "Non-Choir" {
						msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Please select a valid category (Choir/Non-Choir):")
						bot.Send(msg)
						return
					}
					userStates[update.Message.From.ID] = UserState{
						Stage:    "awaiting_lyrics",
						Title:    state.Title,
						Category: update.Message.Text,
					}
					msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Great! Now please enter the lyrics:")
					msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
					bot.Send(msg)
					return

				case "awaiting_lyrics":
					userStates[update.Message.From.ID] = UserState{
						Stage:    "awaiting_image",
						Title:    state.Title,
						Category: state.Category,
						Lyrics:   update.Message.Text,
					}
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"Perfect! Now please send the image URL or upload an image:")
					bot.Send(msg)
					return

				case "awaiting_image":
					imageURL := update.Message.Text
					title := state.Title
					lyrics := state.Lyrics

					_, err := collection.InsertOne(context.TODO(), bson.M{
						"title":    title,
						"lyrics":   lyrics,
						"image":    imageURL,
						"category": state.Category,
					})

					if err != nil {
						msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Failed to add song.")
						bot.Send(msg)
					} else {
						msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Song added successfully!")
						bot.Send(msg)
					}
					delete(userStates, update.Message.From.ID)
					return
				}
			}
		}
		handleAlphabetSelection(bot, update.Message, collection)
	}
}

func lyricsCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, collection *mongo.Collection) {
	songTitle := message.CommandArguments()
	lyrics, imageURL, exists := getLyricsFromDB(collection, songTitle)
	if exists {
		// Send the image
		photoMsg := tgbotapi.NewPhotoShare(message.Chat.ID, imageURL)
		bot.Send(photoMsg)

		// Send the lyrics
		lyricsMsg := tgbotapi.NewMessage(message.Chat.ID, lyrics)
		bot.Send(lyricsMsg)
	} else {
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Sorry, I couldn't find the lyrics for that song."))
	}
}

func getLyricsFromDB(collection *mongo.Collection, title string) (string, string, bool) {
	var result bson.M
	err := collection.FindOne(context.TODO(), bson.M{"title": title}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "", "", false
		}
		log.Printf("Failed to query lyrics: %v", err)
		return "", "", false
	}
	lyrics := result["lyrics"].(string)
	imageURL := result["image"].(string)
	return lyrics, imageURL, true
}

func uploadImageCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	if message.Photo == nil {
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Please attach an image to upload."))
		return
	}

	photo := (*message.Photo)[len(*message.Photo)-1] // Get the highest resolution photo
	fileURL, err := bot.GetFileDirectURL(photo.FileID)
	if err != nil {
		log.Printf("Failed to get file URL: %v", err)
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Failed to process image."))
		return
	}

	imagePath := "temp_image.jpg"
	err = downloadFile(imagePath, fileURL)
	if err != nil {
		log.Printf("Failed to download image: %v", err)
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Failed to download image."))
		return
	}
	defer os.Remove(imagePath) // Clean up the downloaded file

	imgurLink, err := uploadImageToImgur(imagePath)
	if err != nil {
		log.Printf("Failed to upload image to Imgur: %v", err)
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Failed to upload image to Imgur."))
		return
	}

	bot.Send(tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("Image uploaded successfully: %s", imgurLink)))
}

func addSongCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, collection *mongo.Collection) {
	args := strings.SplitN(message.CommandArguments(), "|", 3)
	if len(args) != 3 {
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Usage: /addsong <title>|<lyrics>|<image_url>"))
		return
	}

	title := strings.TrimSpace(args[0])
	lyrics := strings.TrimSpace(args[1])
	imageURL := strings.TrimSpace(args[2])

	_, err := collection.InsertOne(context.TODO(), bson.M{
		"title":  title,
		"lyrics": lyrics,
		"image":  imageURL,
	})
	if err != nil {
		log.Printf("Failed to insert song: %v", err)
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Failed to add song."))
		return
	}

	bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Song added successfully!"))
}

func downloadFile(filepath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func uploadImageToImgur(imagePath string) (string, error) {
	file, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	part, err := writer.CreateFormFile("image", file.Name())
	if err != nil {
		return "", err
	}
	_, err = io.Copy(part, file)
	if err != nil {
		return "", err
	}
	writer.Close()

	req, err := http.NewRequest("POST", "https://api.imgur.com/3/upload", &requestBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Client-ID "+os.Getenv("IMGUR_CLIENT_ID"))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	if data, ok := result["data"].(map[string]interface{}); ok {
		if link, ok := data["link"].(string); ok {
			return link, nil
		}
	}

	return "", fmt.Errorf("failed to upload image: %v", result)
}

func isAdmin(userID int) bool {
	for _, id := range adminIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func helpCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	msg := tgbotapi.NewMessage(message.Chat.ID, "Here are some commands you can use:\n/start - Start the bot\n/help - Get help information\n/lyrics <song title> - Get lyrics for a song")
	bot.Send(msg)
}

func defaultMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message, collection *mongo.Collection) {
	responseText := fmt.Sprintf("You said: %s", message.Text)
	suggestions := getSuggestions(collection, message.Text)
	if len(suggestions) > 0 {
		responseText += "\nDid you mean:\n" + strings.Join(suggestions, "\n")
	}
	msg := tgbotapi.NewMessage(message.Chat.ID, responseText)
	bot.Send(msg)
}

func getSuggestions(collection *mongo.Collection, input string) []string {
	var matches []string
	cursor, err := collection.Find(context.TODO(), bson.M{"title": bson.M{"$regex": "^" + input, "$options": "i"}})
	if err != nil {
		log.Printf("Failed to query suggestions: %v", err)
		return nil
	}
	defer cursor.Close(context.TODO())

	for cursor.Next(context.TODO()) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			log.Printf("Failed to decode result: %v", err)
			return nil
		}
		matches = append(matches, result["title"].(string))
	}
	return matches
}

func handleCallbackQuery(bot *tgbotapi.BotAPI, callbackQuery *tgbotapi.CallbackQuery, collection *mongo.Collection) {
	switch callbackQuery.Data {
	case "popular_series", "new_series", "popular_movies", "new_movies", "popular_anime", "new_anime":
		// Handle the category selection
		msg := tgbotapi.NewMessage(callbackQuery.Message.Chat.ID,
			fmt.Sprintf("You selected: %s\nThis feature is coming soon!", callbackQuery.Data))
		bot.Send(msg)
	default:
		// Handle existing song selection logic
		songTitle := callbackQuery.Data
		lyrics, imageURL, exists := getLyricsFromDB(collection, songTitle)
		if exists {
			photoMsg := tgbotapi.NewPhotoShare(callbackQuery.Message.Chat.ID, imageURL)
			bot.Send(photoMsg)

			msg := tgbotapi.NewMessage(callbackQuery.Message.Chat.ID, lyrics)
			bot.Send(msg)
		} else {
			bot.Send(tgbotapi.NewMessage(callbackQuery.Message.Chat.ID,
				"Sorry, I couldn't find the lyrics for that song."))
		}
	}

	bot.AnswerCallbackQuery(tgbotapi.NewCallback(callbackQuery.ID, ""))
}

func handleAlphabetSelection(bot *tgbotapi.BotAPI, message *tgbotapi.Message, collection *mongo.Collection) {
	alphabet := strings.ToUpper(message.Text)
	if len(alphabet) != 1 || alphabet < "A" || alphabet > "Z" {
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, "Please select a valid alphabet (A-Z)."))
		return
	}

	var songs []string
	cursor, err := collection.Find(context.TODO(), bson.M{"title": bson.M{"$regex": "^" + alphabet, "$options": "i"}})
	if err != nil {
		log.Printf("Failed to query songs: %v", err)
		return
	}
	defer cursor.Close(context.TODO())

	for cursor.Next(context.TODO()) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			log.Printf("Failed to decode result: %v", err)
			return
		}
		songs = append(songs, result["title"].(string))
	}

	if len(songs) == 0 {
		bot.Send(tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("No songs found starting with %s.", alphabet)))
	} else {
		var buttons []tgbotapi.InlineKeyboardButton
		for _, song := range songs {
			buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(song, song))
		}

		keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons)
		msg := tgbotapi.NewMessage(message.Chat.ID, "Select a song to get the lyrics:")
		msg.ReplyMarkup = keyboard
		bot.Send(msg)
	}
}

func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("🎵 Search Lyrics"),
			tgbotapi.NewKeyboardButton("📝 View All Songs"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("👥 Choir Songs"),
			tgbotapi.NewKeyboardButton("🎵 Non-Choir Songs"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("⬆️ Upload Image"),
			tgbotapi.NewKeyboardButton("➕ Add Song"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("✏️ Edit Song"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("❓ Help"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "Welcome to Our Marantha Choir Lyrics Bot! Please select an option:")
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func showSongsByCategory(bot *tgbotapi.BotAPI, message *tgbotapi.Message, collection *mongo.Collection, category string) {
	cursor, err := collection.Find(context.TODO(), bson.M{"category": category})
	if err != nil {
		log.Printf("Failed to query songs: %v", err)
		return
	}
	defer cursor.Close(context.TODO())

	var songs []string
	for cursor.Next(context.TODO()) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			log.Printf("Failed to decode result: %v", err)
			return
		}
		songs = append(songs, result["title"].(string))
	}

	if len(songs) == 0 {
		msg := tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("No %s songs found.", category))
		bot.Send(msg)
		return
	}

	var buttons []tgbotapi.InlineKeyboardButton
	for _, song := range songs {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(song, song))
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons)
	msg := tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("Select a %s song:", category))
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}
