package candidate

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"mime"
	"net/http"
	"time"

	"github.com/dgraph-io/gru/admin/mail"
	"github.com/dgraph-io/gru/admin/server"
	"github.com/dgraph-io/gru/dgraph"
	"github.com/dgraph-io/gru/quiz"
	"github.com/gorilla/mux"
	minio "github.com/minio/minio-go"
)

type Candidate struct {
	Uid       string
	Name      string `json:"name"`
	Email     string `json:"email"`
	Token     string `json:"token"`
	Validity  string `json:"validity"`
	QuizId    string `json:"quiz_id"`
	OldQuizId string `json:"old_quiz_id"`
}

const (
	letterBytes    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	validityLayout = "2006-01-02"
)

// TODO - Optimize later.
func randStringBytes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func index(quizId string) string {
	return `
	{
	quiz(_uid_: ` + quizId + `) {
		quiz.candidate {
			_uid_
			name
			email
			score
			token
			validity
			complete
			deleted
			quiz_start
			invite_sent
			candidate.question {
				candidate.score
			}
		}
	}
}
`
}

func Index(w http.ResponseWriter, r *http.Request) {
	quizId := r.URL.Query().Get("quiz_id")
	sr := server.Response{}
	if quizId == "" {
		sr.Write(w, "", "Quiz id can't be empty.", http.StatusBadRequest)
		return
	}
	q := index(quizId)
	res, err := dgraph.Query(q)
	if err != nil {
		sr.Write(w, "", err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(res)
}

func add(quizId, email string, validity time.Time) server.Response {
	m := new(dgraph.Mutation)
	token := randStringBytes(33)
	m.Set(`<_uid_:` + quizId + `> <quiz.candidate> <_new_:c> .`)
	m.Set(`<_new_:c> <candidate.quiz> <_uid_:` + quizId + `> .`)
	m.Set(`<_new_:c> <email> "` + email + `" .`)
	m.Set(`<_new_:c> <token> "` + token + `" .`)
	m.Set(`<_new_:c> <validity> "` + validity.String() + `" .`)
	m.Set(`<_new_:c> <invite_sent> "` + time.Now().UTC().String() + `" .`)
	m.Set(`<_new_:c> <complete> "false" .`)

	sr := server.Response{}
	mr, err := dgraph.SendMutation(m.String())
	if err != nil {
		sr.Error = err.Error()
		return sr
	}

	// mutation applied successfully, lets send a mail to the candidate.
	uid, ok := mr.Uids["c"]
	if !ok {
		sr.Message = "Uid not returned for newly created candidate."
		return sr
	}

	// Token sent in mail is uid + the random string.
	go mail.Send(email, validity.Format("Mon Jan 2 2006"), uid+token)
	return sr
}

type addCand struct {
	Emails   []string
	Validity string
	QuizId   string `json:"quiz_id"`
}

func Add(w http.ResponseWriter, r *http.Request) {
	sr := server.Response{}
	var c addCand
	err := json.NewDecoder(r.Body).Decode(&c)
	if err != nil {
		sr.Write(w, err.Error(), "Couldn't decode JSON", http.StatusBadRequest)
		return
	}

	var t time.Time
	if t, err = time.Parse(validityLayout, c.Validity); err != nil {
		sr.Write(w, err.Error(), "Couldn't parse the validity", http.StatusBadRequest)
		return
	}

	for _, email := range c.Emails {
		if r := add(c.QuizId, email, t); r.Message != "" || r.Error != "" {
			sr.Write(w, r.Error, r.Message, http.StatusInternalServerError)
			return
		}
	}
	sr.Success = true
	sr.Message = "Candidates invited successfully."
	w.Write(server.MarshalResponse(sr))
}

func edit(c Candidate) string {
	m := new(dgraph.Mutation)
	m.Set(`<_uid_:` + c.Uid + `> <email> "` + c.Email + `" . `)
	m.Set(`<_uid_:` + c.Uid + `> <validity> "` + c.Validity + `" . `)

	// When the quiz for which candidate is invited is changed, we get both OldQuizId
	// and new QuizId.
	if c.QuizId != "" {
		m.Set(`<_uid_:` + c.QuizId + `> <quiz.candidate> <_uid_:` + c.Uid + `> .`)
		m.Set(`<_uid_:` + c.Uid + `> <candidate.quiz> <_uid_:` + c.QuizId + `> .`)
	}
	if c.OldQuizId != "" {
		m.Del(`<_uid_:` + c.OldQuizId + `> <quiz.candidate> <_uid_:` + c.Uid + `> .`)
		m.Del(`<_uid_:` + c.Uid + `> <candidate.quiz> <_uid_:` + c.OldQuizId + `> .`)
	}

	return m.String()
}

// TODO - Changing the quiz for a candidate doesn't work right now. Fix it.
func Edit(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	cid := vars["id"]
	var c Candidate
	sr := server.Response{}
	err := json.NewDecoder(r.Body).Decode(&c)
	if err != nil {
		sr.Write(w, err.Error(), "Couldn't decode JSON", http.StatusBadRequest)
		return
	}

	var t time.Time
	if t, err = time.Parse(validityLayout, c.Validity); err != nil {
		sr.Message = "Couldn't parse the validity"
		sr.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		w.Write(server.MarshalResponse(sr))
		return
	}

	c.Uid = cid
	c.Validity = t.String()
	// TODO - Validate candidate fields shouldn't be empty.
	m := edit(c)
	res, err := dgraph.SendMutation(m)
	if err != nil {
		sr.Write(w, "", err.Error(), http.StatusInternalServerError)
		return
	}
	if res.Code != "ErrorOk" {
		sr.Write(w, res.Message, "Mutation couldn't be applied.",
			http.StatusInternalServerError)
		return
	}
	sr.Success = true
	sr.Message = "Candidate info updated successfully."
	w.Write(server.MarshalResponse(sr))
}

func get(candidateId string) string {
	return `
    {
	quiz.candidate(_uid_:` + candidateId + `) {
		name
		email
		token
		validity
		complete
		candidate.quiz {
			_uid_
			duration
		}
	  }
    }`
}

func Get(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	cid := vars["id"]
	q := get(cid)
	res, err := dgraph.Query(q)
	if err != nil {
		sr := server.Response{}
		sr.Write(w, "", err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(res)
}

type resendReq struct {
	Email    string
	Token    string
	Validity string
}

func ResendInvite(w http.ResponseWriter, r *http.Request) {
	sr := server.Response{}
	vars := mux.Vars(r)
	cid := vars["id"]

	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		sr.Write(w, "", err.Error(), http.StatusBadRequest)
		return
	}
	var rr resendReq
	if err := json.Unmarshal(b, &rr); err != nil {
		sr.Write(w, "", err.Error(), http.StatusBadRequest)
		return
	}

	if rr.Email == "" || rr.Token == "" || rr.Validity == "" {
		sr.Write(w, "", "Email/token/validity can't be empty.", http.StatusBadRequest)
		return
	}

	var t time.Time
	if t, err = time.Parse("2006-01-02 15:04:05 +0000 MST", rr.Validity); err != nil {
		sr.Write(w, err.Error(), "Couldn't parse the validity", http.StatusBadRequest)
		return
	}

	go mail.Send(rr.Email, t.Format("Mon Jan 2 2006"), cid+rr.Token)
	sr.Write(w, "", "Invite has been resent.", http.StatusOK)
}

type candInfo struct {
	Candidates []Candidate `json:"candidate"`
}

func candName(id string) string {
	q := `query {
                candidate(_uid_:` + id + `) {
                        name
                }
        }`
	var ci candInfo
	if err := dgraph.QueryAndUnmarshal(q, &ci); err != nil {
		return ""
	}
	if len(ci.Candidates) != 1 {
		return ""
	}
	return ci.Candidates[0].Name
}

func Resume(w http.ResponseWriter, r *http.Request) {
	sr := server.Response{}
	vars := mux.Vars(r)
	cid := vars["id"]

	s3Client, err := minio.New("s3.amazonaws.com", *quiz.AwsKeyId, *quiz.AwsSecret, true)
	if err != nil {
		sr.Write(w, err.Error(), "", http.StatusInternalServerError)
		return
	}

	object, err := s3Client.GetObject(*quiz.S3bucket, fmt.Sprintf("%v", cid))
	if err != nil {
		sr.Write(w, err.Error(), "", http.StatusInternalServerError)
		return
	}

	defer object.Close()
	stats, err := object.Stat()
	if err != nil {
		sr.Write(w, err.Error(), "", http.StatusInternalServerError)
		return
	}

	ext, err := mime.ExtensionsByType(stats.ContentType)
	if err != nil {
		sr.Write(w, err.Error(), "", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-type", stats.ContentType)
	n := candName(cid)
	w.Header().Set("x-filename", fmt.Sprintf("%v%v", n, ext[0]))
	w.Header().Set("Content-disposition", fmt.Sprintf("attachment;filename=%v%v", cid, ext[0]))
	if _, err := io.Copy(w, object); err != nil {
		sr.Write(w, err.Error(), "", http.StatusInternalServerError)
	}
}
