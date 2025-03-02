// Copyright © 2021 Attestant Limited.
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
	"context"
	"fmt"
	"sync"
	"time"

	eth2client "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/attestantio/vouch/services/accountmanager"
	"github.com/attestantio/vouch/services/metrics"
	"github.com/attestantio/vouch/services/signer"
	"github.com/attestantio/vouch/services/synccommitteeaggregator"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
)

// Service is a sync committee aggregator.
type Service struct {
	monitor                              metrics.SyncCommitteeAggregationMonitor
	slotsPerEpoch                        uint64
	syncCommitteeSize                    uint64
	syncCommitteeSubnetCount             uint64
	targetAggregatorsPerSyncSubcommittee uint64
	beaconBlockRootProvider              eth2client.BeaconBlockRootProvider
	contributionAndProofSigner           signer.ContributionAndProofSigner
	validatingAccountsProvider           accountmanager.ValidatingAccountsProvider
	syncCommitteeContributionProvider    eth2client.SyncCommitteeContributionProvider
	syncCommitteeContributionsSubmitter  eth2client.SyncCommitteeContributionsSubmitter
	beaconBlockRoots                     map[phase0.Slot]phase0.Root
	beaconBlockRootsMu                   sync.Mutex
}

// module-wide log.
var log zerolog.Logger

// New creates a new sync committee aggregator.
func New(ctx context.Context, params ...Parameter) (*Service, error) {
	parameters, err := parseAndCheckParameters(params...)
	if err != nil {
		return nil, errors.Wrap(err, "problem with parameters")
	}

	// Set logging.
	log = zerologger.With().Str("service", "synccommitteeaggregator").Str("impl", "standard").Logger()
	if parameters.logLevel != log.GetLevel() {
		log = log.Level(parameters.logLevel)
	}

	spec, err := parameters.specProvider.Spec(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to obtain spec")
	}

	tmp, exists := spec["SLOTS_PER_EPOCH"]
	if !exists {
		return nil, errors.New("SLOTS_PER_EPOCH not found in spec")
	}
	slotsPerEpoch, ok := tmp.(uint64)
	if !ok {
		return nil, errors.New("SLOTS_PER_EPOCH of unexpected type")
	}

	tmp, exists = spec["SYNC_COMMITTEE_SIZE"]
	if !exists {
		return nil, errors.New("SYNC_COMMITTEE_SIZE not found in spec")
	}
	syncCommitteeSize, ok := tmp.(uint64)
	if !ok {
		return nil, errors.New("SYNC_COMMITTEE_SIZE of unexpected type")
	}

	tmp, exists = spec["SYNC_COMMITTEE_SUBNET_COUNT"]
	if !exists {
		return nil, errors.New("SYNC_COMMITTEE_SUBNET_COUNT not found in spec")
	}
	syncCommitteeSubnetCount, ok := tmp.(uint64)
	if !ok {
		return nil, errors.New("SYNC_COMMITTEE_SUBNET_COUNT of unexpected type")
	}

	tmp, exists = spec["TARGET_AGGREGATORS_PER_SYNC_SUBCOMMITTEE"]
	if !exists {
		return nil, errors.New("TARGET_AGGREGATORS_PER_SYNC_SUBCOMMITTEE not found in spec")
	}
	targetAggregatorsPerSyncSubcommittee, ok := tmp.(uint64)
	if !ok {
		return nil, errors.New("TARGET_AGGREGATORS_PER_SYNC_SUBCOMMITTEE of unexpected type")
	}

	s := &Service{
		monitor:                              parameters.monitor,
		slotsPerEpoch:                        slotsPerEpoch,
		syncCommitteeSize:                    syncCommitteeSize,
		syncCommitteeSubnetCount:             syncCommitteeSubnetCount,
		targetAggregatorsPerSyncSubcommittee: targetAggregatorsPerSyncSubcommittee,
		beaconBlockRootProvider:              parameters.beaconBlockRootProvider,
		contributionAndProofSigner:           parameters.contributionAndProofSigner,
		validatingAccountsProvider:           parameters.validatingAccountsProvider,
		syncCommitteeContributionProvider:    parameters.syncCommitteeContributionProvider,
		syncCommitteeContributionsSubmitter:  parameters.syncCommitteeContributionsSubmitter,
		beaconBlockRoots:                     map[phase0.Slot]phase0.Root{},
	}

	return s, nil
}

// SetBeaconBlockRoot sets the beacon block root used for a given slot.
// Set by the sync committee messenger when it is creating the messages for the slot.
func (s *Service) SetBeaconBlockRoot(slot phase0.Slot, root phase0.Root) {
	s.beaconBlockRootsMu.Lock()
	s.beaconBlockRoots[slot] = root
	s.beaconBlockRootsMu.Unlock()
}

// Aggregate aggregates the attestations for a given slot/committee combination.
func (s *Service) Aggregate(ctx context.Context, data interface{}) {
	ctx, span := otel.Tracer("attestantio.vouch.services.synccommitteeaggregator.standard").Start(ctx, "Aggregate")
	defer span.End()
	started := time.Now()

	duty, ok := data.(*synccommitteeaggregator.Duty)
	if !ok {
		log.Error().Msg("Passed invalid data structure")
		return
	}
	log := log.With().Uint64("slot", uint64(duty.Slot)).Int("validators", len(duty.ValidatorIndices)).Logger()
	log.Trace().Msg("Aggregating")

	var beaconBlockRoot *phase0.Root
	var err error

	s.beaconBlockRootsMu.Lock()
	if tmp, exists := s.beaconBlockRoots[duty.Slot]; exists {
		beaconBlockRoot = &tmp
		delete(s.beaconBlockRoots, duty.Slot)
		s.beaconBlockRootsMu.Unlock()
		log.Trace().Msg("Obtained beacon block root from cache")
	} else {
		s.beaconBlockRootsMu.Unlock()
		log.Debug().Msg("Failed to obtain beacon block root from cache; using head")
		beaconBlockRoot, err = s.beaconBlockRootProvider.BeaconBlockRoot(ctx, "head")
		if err != nil {
			log.Warn().Err(err).Msg("Failed to obtain beacon block root")
			s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(duty.ValidatorIndices), "failed")
			return
		}
		if beaconBlockRoot == nil {
			log.Warn().Msg("Returned empty beacon block root")
			s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(duty.ValidatorIndices), "failed")
			return
		}
	}
	log.Trace().Dur("elapsed", time.Since(started)).Str("beacon_block_root", fmt.Sprintf("%#x", *beaconBlockRoot)).Msg("Obtained beacon block root")

	signedContributionAndProofs := make([]*altair.SignedContributionAndProof, 0)
	for _, validatorIndex := range duty.ValidatorIndices {
		for subcommitteeIndex := range duty.SelectionProofs[validatorIndex] {
			log.Trace().Uint64("validator_index", uint64(validatorIndex)).Uint64("subcommittee_index", subcommitteeIndex).Str("beacon_block_root", fmt.Sprintf("%#x", *beaconBlockRoot)).Msg("Aggregating")
			contribution, err := s.syncCommitteeContributionProvider.SyncCommitteeContribution(ctx, duty.Slot, subcommitteeIndex, *beaconBlockRoot)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to obtain sync committee contribution")
				s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(duty.ValidatorIndices), "failed")
				return
			}
			if contribution == nil {
				log.Warn().Msg("Returned empty contribution")
				s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(duty.ValidatorIndices), "failed")
				return
			}
			contributionAndProof := &altair.ContributionAndProof{
				AggregatorIndex: validatorIndex,
				Contribution:    contribution,
				SelectionProof:  duty.SelectionProofs[validatorIndex][subcommitteeIndex],
			}
			sig, err := s.contributionAndProofSigner.SignContributionAndProof(ctx, duty.Accounts[validatorIndex], contributionAndProof)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to obtain signature of contribution and proof")
				s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(duty.ValidatorIndices), "failed")
				return
			}

			signedContributionAndProof := &altair.SignedContributionAndProof{
				Message:   contributionAndProof,
				Signature: sig,
			}

			signedContributionAndProofs = append(signedContributionAndProofs, signedContributionAndProof)
		}
	}

	if err := s.syncCommitteeContributionsSubmitter.SubmitSyncCommitteeContributions(ctx, signedContributionAndProofs); err != nil {
		log.Warn().Err(err).Msg("Failed to submit signed contribution and proofs")
		s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(signedContributionAndProofs), "failed")
		return
	}

	log.Trace().Msg("Submitted signed contribution and proofs")
	for i := range signedContributionAndProofs {
		frac := float64(signedContributionAndProofs[i].Message.Contribution.AggregationBits.Count()) /
			float64(signedContributionAndProofs[i].Message.Contribution.AggregationBits.Len())
		s.monitor.SyncCommitteeAggregationCoverage(frac)
	}
	s.monitor.SyncCommitteeAggregationsCompleted(started, duty.Slot, len(signedContributionAndProofs), "succeeded")
}
