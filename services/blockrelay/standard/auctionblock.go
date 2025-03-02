// Copyright © 2022 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/attestantio/go-block-relay/services/blockauctioneer"
	builderclient "github.com/attestantio/go-builder-client"
	builderspec "github.com/attestantio/go-builder-client/spec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/attestantio/vouch/services/beaconblockproposer"
	"github.com/attestantio/vouch/util"
	"github.com/holiman/uint256"
	"github.com/pkg/errors"
	e2types "github.com/wealdtech/go-eth2-types/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// zeroExecutionAddress is used for comparison purposes.
var zeroExecutionAddress bellatrix.ExecutionAddress

// zeroValue is used for comparison purposes.
var zeroValue uint256.Int

// AuctionBlock obtains the best available use of the block space.
func (s *Service) AuctionBlock(ctx context.Context,
	slot phase0.Slot,
	parentHash phase0.Hash32,
	pubkey phase0.BLSPubKey,
) (
	*blockauctioneer.Results,
	error,
) {
	ctx, span := otel.Tracer("attestantio.vouch.services.blockrelay.standard").Start(ctx, "AuctionBlock")
	defer span.End()

	account, err := s.accountsProvider.AccountByPublicKey(ctx, pubkey)
	if err != nil {
		return nil, errors.New("no account found for public key")
	}
	s.executionConfigMu.RLock()
	proposerConfig, err := s.executionConfig.ProposerConfig(ctx, account, pubkey, s.fallbackFeeRecipient, s.fallbackGasLimit)
	if err != nil {
		return nil, errors.Wrap(err, "failed to obtain proposer configuration")
	}
	s.executionConfigMu.RUnlock()

	if len(proposerConfig.Relays) == 0 {
		log.Trace().Msg("No relays in proposer configuration")
		return nil, nil
	}

	res := s.bestBuilderBid(ctx, slot, parentHash, pubkey, proposerConfig)
	if res == nil {
		return nil, nil
	}

	if res.Bid != nil {
		key := fmt.Sprintf("%d", slot)
		subKey := fmt.Sprintf("%x:%x", parentHash, pubkey)
		s.builderBidsCacheMu.Lock()
		if _, exists := s.builderBidsCache[key]; !exists {
			s.builderBidsCache[key] = make(map[string]*builderspec.VersionedSignedBuilderBid)
		}
		s.builderBidsCache[key][subKey] = res.Bid
		s.builderBidsCacheMu.Unlock()
	}

	selectedProviders := make(map[string]struct{})
	for _, provider := range res.Providers {
		selectedProviders[strings.ToLower(provider.Address())] = struct{}{}
	}

	// Update metrics.
	val, err := res.Bid.Value()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to obtain bid value")
	} else {
		for provider, value := range res.Values {
			delta := new(big.Int).Sub(val.ToBig(), value)
			_, isSelected := selectedProviders[strings.ToLower(provider)]
			if !isSelected {
				monitorBuilderBidDelta(provider, delta)
			}
			if s.logResults {
				log.Info().Uint64("slot", uint64(slot)).Str("provider", provider).Stringer("value", value).Stringer("delta", delta).Bool("selected", isSelected).Msg("Auction participant")
			} else {
				log.Trace().Uint64("slot", uint64(slot)).Str("provider", provider).Stringer("value", value).Stringer("delta", delta).Bool("selected", isSelected).Msg("Auction participant")
			}
		}
	}

	return res, nil
}

type builderBidResponse struct {
	provider builderclient.BuilderBidProvider
	bid      *builderspec.VersionedSignedBuilderBid
	score    *big.Int
}

// bestBuilderBid provides the best builder bid from a number of relays.
func (s *Service) bestBuilderBid(ctx context.Context,
	slot phase0.Slot,
	parentHash phase0.Hash32,
	pubkey phase0.BLSPubKey,
	proposerConfig *beaconblockproposer.ProposerConfig,
) *blockauctioneer.Results {
	ctx, span := otel.Tracer("attestantio.vouch.services.blockrelay.standard").Start(ctx, "bestBuilderBid")
	defer span.End()
	started := time.Now()
	log := util.LogWithID(ctx, log, "strategy_id").With().Str("operation", "builderbid").Uint64("slot", uint64(slot)).Str("pubkey", fmt.Sprintf("%#x", pubkey)).Logger()

	res := &blockauctioneer.Results{
		Values:    make(map[string]*big.Int),
		Providers: make([]builderclient.BuilderBidProvider, 0),
	}
	requests := len(proposerConfig.Relays)

	// We have two timeouts: a soft timeout and a hard timeout.
	// At the soft timeout, we return if we have any responses so far.
	// At the hard timeout, we return unconditionally.
	// The soft timeout is half the duration of the hard timeout.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	softCtx, softCancel := context.WithTimeout(ctx, s.timeout/2)

	respCh := make(chan *builderBidResponse, requests)
	errCh := make(chan error, requests)
	// Kick off the requests.
	for _, relay := range proposerConfig.Relays {
		builderClient, err := util.FetchBuilderClient(ctx, relay.Address, s.monitor)
		if err != nil {
			// Error but continue.
			log.Error().Err(err).Msg("Failed to obtain builder client for block auction")
			continue
		}
		provider, isProvider := builderClient.(builderclient.BuilderBidProvider)
		if !isProvider {
			// Error but continue.
			log.Error().Err(err).Msg("Builder client does not supply builder bids")
			continue
		}
		go s.builderBid(ctx, provider, respCh, errCh, slot, parentHash, pubkey, relay)
	}

	// Wait for all responses (or context done).
	responded := 0
	errored := 0
	timedOut := 0
	softTimedOut := 0
	bestScore := big.NewInt(0)

	// Loop 1: prior to soft timeout.
	for responded+errored+timedOut+softTimedOut != requests {
		select {
		case resp := <-respCh:
			responded++
			log.Trace().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Msg("Response received")
			if resp.bid == nil {
				// This means that the bid was ineligible, for example the bid value was too small.
				continue
			}
			switch {
			case resp.score.Cmp(bestScore) > 0:
				log.Trace().Str("provider", resp.provider.Address()).Stringer("score", resp.score).Msg("New winning bid")
				res.Bid = resp.bid
				bestScore = resp.score
				res.Providers = []builderclient.BuilderBidProvider{resp.provider}
			case res.Bid != nil && resp.score.Cmp(bestScore) == 0 && bidsEqual(res.Bid, resp.bid):
				log.Trace().Str("provider", resp.provider.Address()).Msg("Matching bid from different relay")
				res.Providers = append(res.Providers, resp.provider)
			default:
				log.Trace().Str("provider", resp.provider.Address()).Stringer("score", resp.score).Msg("Low or slow bid")
			}
			res.Values[resp.provider.Address()] = resp.score
		case err := <-errCh:
			errored++
			log.Debug().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Err(err).Msg("Error received")
		case <-softCtx.Done():
			// If we have any responses at this point we consider the non-responders timed out.
			if responded > 0 {
				timedOut = requests - responded - errored
				log.Debug().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Msg("Soft timeout reached with responses")
			} else {
				log.Debug().Dur("elapsed", time.Since(started)).Int("errored", errored).Msg("Soft timeout reached with no responses")
			}
			// Set the number of requests that have soft timed out.
			softTimedOut = requests - responded - errored - timedOut
		}
	}
	softCancel()

	// Loop 2: after soft timeout.
	for responded+errored+timedOut != requests {
		select {
		case resp := <-respCh:
			responded++
			log.Trace().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Msg("Response received")
			if resp.bid == nil {
				// This means that the bid was ineligible, for example the bid value was too small.
				continue
			}
			switch {
			case resp.score.Cmp(bestScore) > 0:
				log.Trace().Str("provider", resp.provider.Address()).Stringer("score", resp.score).Msg("New winning bid")
				res.Bid = resp.bid
				bestScore = resp.score
				res.Providers = []builderclient.BuilderBidProvider{resp.provider}
			case res.Bid != nil && resp.score.Cmp(bestScore) == 0 && bidsEqual(res.Bid, resp.bid):
				log.Trace().Str("provider", resp.provider.Address()).Msg("Matching bid from different relay")
				res.Providers = append(res.Providers, resp.provider)
			default:
				log.Trace().Str("provider", resp.provider.Address()).Stringer("score", resp.score).Msg("Low or slow bid")
			}
			res.Values[resp.provider.Address()] = resp.score
		case err := <-errCh:
			errored++
			log.Debug().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Err(err).Msg("Error received")
		case <-ctx.Done():
			// Anyone not responded by now is considered errored.
			timedOut = requests - responded - errored
			log.Debug().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Msg("Hard timeout reached")
		}
	}
	cancel()
	log.Trace().Dur("elapsed", time.Since(started)).Int("responded", responded).Int("errored", errored).Int("timed_out", timedOut).Msg("Results")

	if res.Bid == nil {
		log.Debug().Msg("No useful bids received")
		monitorAuctionBlock("", false, time.Since(started))
		return nil
	}

	log.Trace().Stringer("bid", res.Bid).Msg("Selected best bid")

	for _, provider := range res.Providers {
		monitorAuctionBlock(provider.Address(), true, time.Since(started))
	}

	return res
}

func (s *Service) builderBid(ctx context.Context,
	provider builderclient.BuilderBidProvider,
	respCh chan *builderBidResponse,
	errCh chan error,
	slot phase0.Slot,
	parentHash phase0.Hash32,
	pubkey phase0.BLSPubKey,
	relayConfig *beaconblockproposer.RelayConfig,
) {
	ctx, span := otel.Tracer("attestantio.vouch.services.blockrelay.standard").Start(ctx, "builderBid", trace.WithAttributes(
		attribute.String("relay", provider.Address()),
	))
	defer span.End()

	if relayConfig.Grace > 0 {
		time.Sleep(relayConfig.Grace)
		span.AddEvent("grace period over")
	}

	log := log.With().Str("bidder", provider.Address()).Logger()
	builderBid, err := provider.BuilderBid(ctx, slot, parentHash, pubkey)
	if err != nil {
		errCh <- errors.Wrap(err, provider.Address())
		return
	}
	if builderBid == nil {
		respCh <- &builderBidResponse{
			provider: provider,
			score:    big.NewInt(0),
		}
		return
	}
	if e := log.Trace(); e.Enabled() {
		data, err := json.Marshal(builderBid)
		if err != nil {
			errCh <- errors.Wrap(err, provider.Address())
			return
		}
		e.RawJSON("builder_bid", data).Msg("Obtained builder bid")
	}
	if builderBid.IsEmpty() {
		errCh <- fmt.Errorf("%s: builder bid empty", provider.Address())
		return
	}

	value, err := builderBid.Value()
	if err != nil {
		errCh <- fmt.Errorf("%s: invalid value", provider.Address())
		return
	}
	if zeroValue.Cmp(value) == 0 {
		errCh <- fmt.Errorf("%s: zero value", provider.Address())
		return
	}
	if value.ToBig().Cmp(relayConfig.MinValue.BigInt()) < 0 {
		log.Debug().Stringer("value", value.ToBig()).Stringer("min_value", relayConfig.MinValue.BigInt()).Msg("Value below minimum; ignoring")
		respCh <- &builderBidResponse{
			provider: provider,
			score:    big.NewInt(0),
		}
		return
	}

	feeRecipient, err := builderBid.FeeRecipient()
	if err != nil {
		errCh <- fmt.Errorf("%s: fee recipient: %w", provider.Address(), err)
		return
	}
	if bytes.Equal(feeRecipient[:], zeroExecutionAddress[:]) {
		errCh <- fmt.Errorf("%s: zero fee recipient", provider.Address())
		return
	}

	timestamp, err := builderBid.Timestamp()
	if err != nil {
		errCh <- fmt.Errorf("%s: timestamp: %w", provider.Address(), err)
		return
	}
	if uint64(s.chainTime.StartOfSlot(slot).Unix()) != timestamp {
		errCh <- fmt.Errorf("%s: provided timestamp %d for slot %d not expected value of %d", provider.Address(), timestamp, slot, s.chainTime.StartOfSlot(slot).Unix())
		return
	}

	verified, err := s.verifyBidSignature(ctx, relayConfig, builderBid, provider)
	if err != nil {
		errCh <- errors.Wrap(err, "error verifying bid signature")
		return
	}
	if !verified {
		log.Warn().Msg("Failed to verify bid signature")
		errCh <- fmt.Errorf("%s: invalid signature", provider.Address())
		return
	}

	respCh <- &builderBidResponse{
		bid:      builderBid,
		provider: provider,
		score:    value.ToBig(),
	}
}

// verifyBidSignature verifies the signature of a bid to ensure it comes from the expected source.
func (s *Service) verifyBidSignature(_ context.Context,
	relayConfig *beaconblockproposer.RelayConfig,
	bid *builderspec.VersionedSignedBuilderBid,
	provider builderclient.BuilderBidProvider,
) (
	bool,
	error,
) {
	var err error
	log := log.With().Str("provider", provider.Address()).Logger()

	relayPubkey := relayConfig.PublicKey
	if relayPubkey == nil {
		// Try to fetch directly from the provider.
		relayPubkey = provider.Pubkey()
		if relayPubkey == nil {
			log.Trace().Msg("Relay configuration does not contain public key; skipping validation")
			return true, nil
		}
	}

	s.relayPubkeysMu.RLock()
	pubkey, exists := s.relayPubkeys[*relayPubkey]
	s.relayPubkeysMu.RUnlock()
	if !exists {
		pubkey, err = e2types.BLSPublicKeyFromBytes(relayPubkey[:])
		if err != nil {
			return false, errors.Wrap(err, "invalid public key supplied with bid")
		}
		s.relayPubkeysMu.Lock()
		s.relayPubkeys[*relayPubkey] = pubkey
		s.relayPubkeysMu.Unlock()
	}

	dataRoot, err := bid.MessageHashTreeRoot()
	if err != nil {
		return false, errors.Wrap(err, "failed to hash bid message")
	}

	signingData := &phase0.SigningData{
		ObjectRoot: dataRoot,
		Domain:     s.applicationBuilderDomain,
	}
	signingRoot, err := signingData.HashTreeRoot()
	if err != nil {
		return false, errors.Wrap(err, "failed to hash signing data")
	}

	bidSig, err := bid.Signature()
	if err != nil {
		return false, errors.Wrap(err, "failed to obtain bid signature")
	}

	byteSig := make([]byte, len(bidSig))
	copy(byteSig, bidSig[:])
	sig, err := e2types.BLSSignatureFromBytes(byteSig)
	if err != nil {
		return false, errors.Wrap(err, "invalid signature")
	}

	verified := sig.Verify(signingRoot[:], pubkey)
	if !verified {
		data, err := json.Marshal(bid)
		if err == nil {
			log.Debug().RawJSON("bid", data).Msg("Verification failure")
		}
	}

	return verified, nil
}

// bidsEqual returns true if the two bids are equal.
// Bids are considered equal if they have the same header.
// Note that this function is only called if the bids have the same value, so that is not checked here.
func bidsEqual(bid1 *builderspec.VersionedSignedBuilderBid, bid2 *builderspec.VersionedSignedBuilderBid) bool {
	bid1Root, err := bid1.HeaderHashTreeRoot()
	if err != nil {
		return false
	}
	bid2Root, err := bid2.HeaderHashTreeRoot()
	if err != nil {
		return false
	}
	return bytes.Equal(bid1Root[:], bid2Root[:])
}
