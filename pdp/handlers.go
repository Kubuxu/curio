package pdp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/filecoin-project/curio/pdp/contract"
	"github.com/filecoin-project/curio/tasks/message"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/paths"
)

// PDPRoutePath is the base path for PDP routes
const PDPRoutePath = "/pdp"

// PDPService represents the service for managing proof sets and pieces
type PDPService struct {
	db      *harmonydb.DB
	storage paths.StashStore

	sender    *message.SenderETH
	ethClient *ethclient.Client

	ProofSetStore     ProofSetStore
	PieceStore        PieceStore
	OwnerAddressStore OwnerAddressStore
}

// NewPDPService creates a new instance of PDPService with the provided stores
func NewPDPService(db *harmonydb.DB, stor paths.StashStore, ec *ethclient.Client, sn *message.SenderETH) *PDPService {
	return &PDPService{
		db:      db,
		storage: stor,

		sender:    sn,
		ethClient: ec,
	}
}

// Routes registers the HTTP routes with the provided router
func Routes(r *chi.Mux, p *PDPService) {

	// Routes for proof sets
	r.Route(path.Join(PDPRoutePath, "/proof-sets"), func(r chi.Router) {
		// POST /pdp/proof-sets - Create a new proof set
		r.Post("/", p.handleCreateProofSet)

		// Individual proof set routes
		r.Route("/{proofSetID}", func(r chi.Router) {
			// GET /pdp/proof-sets/{set-id}
			r.Get("/", p.handleGetProofSet)

			// DEL /pdp/proof-sets/{set-id}
			r.Delete("/", p.handleDeleteProofSet)

			// Routes for roots within a proof set
			r.Route("/roots", func(r chi.Router) {
				// POST /pdp/proof-sets/{set-id}/roots
				r.Post("/", p.handleAddRootToProofSet)

				// Individual root routes
				r.Route("/{rootID}", func(r chi.Router) {
					// GET /pdp/proof-sets/{set-id}/roots/{root-id}
					r.Get("/", p.handleGetProofSetRoot)

					// DEL /pdp/proof-sets/{set-id}/roots/{root-id}
					r.Delete("/", p.handleDeleteProofSetRoot)
				})
			})
		})
	})

	r.Get(path.Join(PDPRoutePath, "/ping"), p.handlePing)

	// Routes for piece storage and retrieval
	// POST /pdp/piece
	r.Post(path.Join(PDPRoutePath, "/piece"), p.handlePiecePost)

	// PUT /pdp/piece/upload/{uploadUUID}
	r.Put(path.Join(PDPRoutePath, "/piece/upload/{uploadUUID}"), p.handlePieceUpload)
}

// Handler functions

func (p *PDPService) handlePing(w http.ResponseWriter, r *http.Request) {
	// Verify that the request is authorized using ECDSA JWT
	_, err := p.verifyJWTToken(r)
	if err != nil {
		http.Error(w, "Unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	// Return 200 OK
	w.WriteHeader(http.StatusOK)
}

// TODO STUFF BELOW IS JUST A SKELETON

// handleCreateProofSet handles the creation of a new proof set
func (p *PDPService) handleCreateProofSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Step 1: Verify that the request is authorized using ECDSA JWT
	serviceLabel, err := p.verifyJWTToken(r)
	if err != nil {
		http.Error(w, "Unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	// Step 2: Parse the request body to get the 'recordKeeper' address
	type RequestBody struct {
		RecordKeeper string `json:"recordKeeper"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var reqBody RequestBody
	if err := json.Unmarshal(body, &reqBody); err != nil {
		http.Error(w, "Invalid JSON in request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if reqBody.RecordKeeper == "" {
		http.Error(w, "recordKeeper address is required", http.StatusBadRequest)
		return
	}

	recordKeeperAddr := common.HexToAddress(reqBody.RecordKeeper)
	if recordKeeperAddr == (common.Address{}) {
		http.Error(w, "Invalid recordKeeper address", http.StatusBadRequest)
		return
	}

	// Step 3: Get the sender address from 'eth_keys' table where role = 'pdp' limit 1
	fromAddress, err := p.getSenderAddress(ctx)
	if err != nil {
		http.Error(w, "Failed to get sender address: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Step 4: Prepare the transaction to call 'createProofSet' method
	contracts := contract.ContractAddresses()
	pdpServiceAddr := contracts.PDPService

	// Create a bound contract instance
	pdpServiceContract, err := contract.NewPDPService(pdpServiceAddr, p.ethClient)
	if err != nil {
		http.Error(w, "Failed to bind PDPService contract: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create TransactOpts without a Signer to prevent automatic signing and sending
	transactor := &bind.TransactOpts{
		From:    fromAddress,
		Context: ctx,
		Signer:  nil,  // We leave the Signer as nil because SenderETH handles signing
		NoSend:  true, // Do not send the transaction immediately
	}

	// Prepare the transaction (but do not send it)
	tx, err := pdpServiceContract.CreateProofSet(transactor, recordKeeperAddr)
	if err != nil {
		http.Error(w, "Failed to create transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Step 5: Send the transaction using SenderETH
	reason := "pdp-mkproofset"
	txHash, err := p.sender.Send(ctx, fromAddress, tx, reason)
	if err != nil {
		http.Error(w, "Failed to send transaction: "+err.Error(), http.StatusInternalServerError)
		log.Errorf("Failed to send transaction: %+v", err)
		return
	}

	// Step 6: Insert into message_waits_eth and pdp_proofset_creates
	err = p.insertMessageWaitsAndProofsetCreate(ctx, txHash.Hex(), serviceLabel)
	if err != nil {
		log.Errorf("Failed to insert into message_waits_eth and pdp_proofset_creates: %+v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Step 7: Respond with 201 Created and Location header
	w.Header().Set("Location", path.Join("/pdp/proof-sets/created", txHash.Hex()))
	w.WriteHeader(http.StatusCreated)
}

// getSenderAddress retrieves the sender address from the database where role = 'pdp' limit 1
func (p *PDPService) getSenderAddress(ctx context.Context) (common.Address, error) {
	var addressStr string
	err := p.db.QueryRow(ctx, `SELECT address FROM eth_keys WHERE role = 'pdp' LIMIT 1`).Scan(&addressStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return common.Address{}, errors.New("no sender address with role 'pdp' found")
		}
		return common.Address{}, err
	}
	address := common.HexToAddress(addressStr)
	return address, nil
}

// insertMessageWaitsAndProofsetCreate inserts records into message_waits_eth and pdp_proofset_creates
func (p *PDPService) insertMessageWaitsAndProofsetCreate(ctx context.Context, txHashHex string, serviceLabel string) error {
	// Begin a database transaction
	_, err := p.db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		// Insert into message_waits_eth
		_, err := tx.Exec(`
            INSERT INTO message_waits_eth (signed_tx_hash, tx_status)
            VALUES ($1, $2)
        `, txHashHex, "pending")
		if err != nil {
			return false, err // Return false to rollback the transaction
		}

		// Insert into pdp_proofset_creates
		_, err = tx.Exec(`
            INSERT INTO pdp_proofset_creates (create_message_hash, service)
            VALUES ($1, $2)
        `, txHashHex, serviceLabel)
		if err != nil {
			return false, err // Return false to rollback the transaction
		}

		// Return true to commit the transaction
		return true, nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (p *PDPService) handleGetProofSet(w http.ResponseWriter, r *http.Request) {
	// Spec snippet:
	// ### GET /proof-sets/{set-id}
	// Response:
	// Code: 200
	// Body:
	// {
	//   "id": "{set-id}",
	//   "nextChallengeEpoch": 15,
	//   "roots": [
	//     // Root details
	//   ]
	// }

	proofSetIDStr := chi.URLParam(r, "proofSetID")
	proofSetID, err := strconv.ParseInt(proofSetIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid proof set ID", http.StatusBadRequest)
		return
	}

	// Retrieve proof set from store
	proofSetDetails, err := p.ProofSetStore.GetProofSet(proofSetID)
	if err != nil {
		http.Error(w, "Proof set not found", http.StatusNotFound)
		return
	}

	// Implement authorization if necessary

	// Respond with proof set details
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(proofSetDetails)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

func (p *PDPService) handleDeleteProofSet(w http.ResponseWriter, r *http.Request) {
	// Spec snippet:
	// ### DEL /proof-sets/{set id}
	// Remove the specified proof set entirely

	proofSetIDStr := chi.URLParam(r, "proofSetID")
	proofSetID, err := strconv.ParseInt(proofSetIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid proof set ID", http.StatusBadRequest)
		return
	}

	// Implement authorization (e.g., only the owner can delete)

	err = p.ProofSetStore.DeleteProofSet(proofSetID)
	if err != nil {
		http.Error(w, "Failed to delete proof set", http.StatusInternalServerError)
		return
	}

	// Respond with 204 No Content
	w.WriteHeader(http.StatusNoContent)
}

func (p *PDPService) handleAddRootToProofSet(w http.ResponseWriter, r *http.Request) {
	// Spec snippet:
	// ### POST /proof-sets/{set-id}/roots
	// Append a root to the proof set
	// Request Body:
	// {
	//   "rootId": {root ID},
	//   "rootCid": "bafy....root",
	//   "subroots": [
	//     {
	//       "subrootCid": "bafy...subroot",
	//       "subrootOffset": 0,
	//       "pieceCid": "bafy...piece1"
	//     },
	//     //...
	//   ]
	// }

	proofSetIDStr := chi.URLParam(r, "proofSetID")
	proofSetID, err := strconv.ParseInt(proofSetIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid proof set ID", http.StatusBadRequest)
		return
	}

	// Parse request body
	var req struct {
		RootID   int64  `json:"rootId"`
		RootCID  string `json:"rootCid"`
		Subroots []struct {
			SubrootCID    string `json:"subrootCid"`
			SubrootOffset int64  `json:"subrootOffset"`
			PieceCID      string `json:"pieceCid"`
		} `json:"subroots"`
	}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil || req.RootCID == "" || len(req.Subroots) == 0 {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Implement authorization (e.g., only owner can add roots)

	// For each subroot, check that the piece exists and get the pdp_pieceref ID
	for _, subroot := range req.Subroots {
		// Check if piece exists
		exists, err := p.PieceStore.HasPiece(subroot.PieceCID)
		if err != nil {
			http.Error(w, "Error checking piece existence", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "Piece not found: "+subroot.PieceCID, http.StatusBadRequest)
			return
		}

		// Get the pdp_pieceref ID for this piece
		pieceRefID, err := p.PieceStore.GetPieceRefIDByPieceCID(subroot.PieceCID)
		if err != nil {
			http.Error(w, "Failed to get piece reference for "+subroot.PieceCID, http.StatusInternalServerError)
			return
		}

		// Create the proof set root entry
		proofSetRoot := &PDPProofSetRoot{
			ProofSetID:    proofSetID,
			RootID:        req.RootID,
			Root:          req.RootCID,
			Subroot:       subroot.SubrootCID,
			SubrootOffset: subroot.SubrootOffset,
			PDPPieceRefID: pieceRefID,
		}

		// Add to proof set store
		err = p.ProofSetStore.AddProofSetRoot(proofSetRoot)
		if err != nil {
			http.Error(w, "Failed to add root to proof set", http.StatusInternalServerError)
			return
		}
	}

	// Set Location header
	w.Header().Set("Location", path.Join(PDPRoutePath, "/proof-sets", fmt.Sprintf("%d", proofSetID), "roots", fmt.Sprintf("%d", req.RootID)))

	// Set status code to 201 Created
	w.WriteHeader(http.StatusCreated)
}

func (p *PDPService) handleGetProofSetRoot(w http.ResponseWriter, r *http.Request) {
	// Spec snippet:
	// ### GET /proof-sets/{set id}/roots/{root id}
	// Response Body:
	// {
	//   "rootId": {root ID},
	//   "rootCid": "bafy....root",
	//   "subroots": [
	//     {
	//       "subrootCid": "bafy...subroot",
	//       "subrootOffset": 0,
	//       "pieceCid": "bafy...piece1"
	//     },
	//     //...
	//   ]
	// }

	/*	proofSetIDStr := chi.URLParam(r, "proofSetID")
		proofSetID, err := strconv.ParseInt(proofSetIDStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid proof set ID", http.StatusBadRequest)
			return
		}

		rootIDStr := chi.URLParam(r, "rootID")
		rootID, err := strconv.ParseInt(rootIDStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid root ID", http.StatusBadRequest)
			return
		}*/

	// Retrieve root from proof set in store
	/*rootDetails, err := p.ProofSetStore.GetProofSetRoot(proofSetID, rootID)
	if err != nil {
		http.Error(w, "Root not found", http.StatusNotFound)
		return
	}*/

	// Respond with root details
	w.Header().Set("Content-Type", "application/json")
	/*err = json.NewEncoder(w).Encode(rootDetails)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}*/
}

func (p *PDPService) handleDeleteProofSetRoot(w http.ResponseWriter, r *http.Request) {
	// Spec snippet:
	// ### DEL /proof-sets/{set id}/roots/{root id}

	proofSetIDStr := chi.URLParam(r, "proofSetID")
	proofSetID, err := strconv.ParseInt(proofSetIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid proof set ID", http.StatusBadRequest)
		return
	}

	rootIDStr := chi.URLParam(r, "rootID")
	rootID, err := strconv.ParseInt(rootIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid root ID", http.StatusBadRequest)
		return
	}

	// Implement authorization (e.g., only owner can delete roots)

	// Delete root from proof set in store
	err = p.ProofSetStore.DeleteProofSetRoot(proofSetID, rootID)
	if err != nil {
		http.Error(w, "Failed to delete root", http.StatusInternalServerError)
		return
	}

	// Respond with 204 No Content
	w.WriteHeader(http.StatusNoContent)
}

// Data models corresponding to the updated schema

// PDPOwnerAddress represents the owner address with its private key
type PDPOwnerAddress struct {
	OwnerAddress string // PRIMARY KEY
	PrivateKey   []byte // BYTEA NOT NULL
}

// PDPServiceEntry represents a PDP service entry
type PDPServiceEntry struct {
	ID           int64     // PRIMARY KEY
	PublicKey    []byte    // BYTEA NOT NULL
	ServiceLabel string    // TEXT NOT NULL
	CreatedAt    time.Time // DEFAULT CURRENT_TIMESTAMP
}

// PDPPieceRef represents a PDP piece reference
type PDPPieceRef struct {
	ID         int64     // PRIMARY KEY
	ServiceID  int64     // pdp_services.id
	PieceCID   string    // TEXT NOT NULL
	RefID      string    // TEXT NOT NULL
	ServiceTag string    // VARCHAR(64)
	ClientTag  string    // VARCHAR(64)
	CreatedAt  time.Time // DEFAULT CURRENT_TIMESTAMP
}

// PDPProofSet represents a proof set
type PDPProofSet struct {
	ID                 int64 // PRIMARY KEY (on-chain proofset id)
	NextChallengeEpoch int64 // Cached chain value
}

// PDPProofSetRoot represents a root in a proof set
type PDPProofSetRoot struct {
	ProofSetID    int64  // proofset BIGINT NOT NULL
	RootID        int64  // root_id BIGINT NOT NULL
	Root          string // root TEXT NOT NULL
	Subroot       string // subroot TEXT NOT NULL
	SubrootOffset int64  // subroot_offset BIGINT NOT NULL
	PDPPieceRefID int64  // pdp_piecerefs.id
}

// PDPProveTask represents a prove task
type PDPProveTask struct {
	ProofSetID     int64  // proofset
	ChallengeEpoch int64  // challenge epoch
	TaskID         int64  // harmonytask task ID
	MessageCID     string // text
	MessageEthHash string // text
}

// Interfaces

// ProofSetStore defines methods to manage proof sets and roots
type ProofSetStore interface {
	CreateProofSet(proofSet *PDPProofSet) (int64, error)
	GetProofSet(proofSetID int64) (*PDPProofSetDetails, error)
	DeleteProofSet(proofSetID int64) error
	AddProofSetRoot(proofSetRoot *PDPProofSetRoot) error
	DeleteProofSetRoot(proofSetID int64, rootID int64) error
}

// PieceStore defines methods to manage pieces and piece references
type PieceStore interface {
	HasPiece(pieceCID string) (bool, error)
	StorePiece(pieceCID string, data []byte) error
	GetPiece(pieceCID string) ([]byte, error)
	GetPieceRefIDByPieceCID(pieceCID string) (int64, error)
}

// OwnerAddressStore defines methods to manage owner addresses
type OwnerAddressStore interface {
	HasOwnerAddress(ownerAddress string) (bool, error)
}

// PDPProofSetDetails represents the details of a proof set, including roots
type PDPProofSetDetails struct {
	ID                 int64                   `json:"id"`
	NextChallengeEpoch int64                   `json:"nextChallengeEpoch"`
	Roots              []PDPProofSetRootDetail `json:"roots"`
}

// PDPProofSetRootDetail represents the details of a root in a proof set
type PDPProofSetRootDetail struct {
	RootID   int64                      `json:"rootId"`
	RootCID  string                     `json:"rootCid"`
	Subroots []PDPProofSetSubrootDetail `json:"subroots"`
}

// PDPProofSetSubrootDetail represents a subroot in a proof set root
type PDPProofSetSubrootDetail struct {
	SubrootCID    string `json:"subrootCid"`
	SubrootOffset int64  `json:"subrootOffset"`
	PieceCID      string `json:"pieceCid"`
}