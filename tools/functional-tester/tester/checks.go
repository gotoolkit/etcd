// Copyright 2018 The etcd Authors
//
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

package tester

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	"github.com/coreos/etcd/tools/functional-tester/rpcpb"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const retries = 7

// Checker checks cluster consistency.
type Checker interface {
	// Check returns an error if the system fails a consistency check.
	Check() error
}

type hashAndRevGetter interface {
	getRevisionHash() (revs map[string]int64, hashes map[string]int64, err error)
}

type hashChecker struct {
	lg  *zap.Logger
	hrg hashAndRevGetter
}

func newHashChecker(lg *zap.Logger, hrg hashAndRevGetter) Checker {
	return &hashChecker{
		lg:  lg,
		hrg: hrg,
	}
}

const leaseCheckerTimeout = 10 * time.Second

func (hc *hashChecker) checkRevAndHashes() (err error) {
	var (
		revs   map[string]int64
		hashes map[string]int64
	)
	// retries in case of transient failure or etcd cluster has not stablized yet.
	for i := 0; i < retries; i++ {
		revs, hashes, err = hc.hrg.getRevisionHash()
		if err != nil {
			hc.lg.Warn(
				"failed to get revision and hash",
				zap.Int("retries", i),
				zap.Error(err),
			)
		} else {
			sameRev := getSameValue(revs)
			sameHashes := getSameValue(hashes)
			if sameRev && sameHashes {
				return nil
			}
			hc.lg.Warn(
				"retrying; etcd cluster is not stable",
				zap.Int("retries", i),
				zap.Bool("same-revisions", sameRev),
				zap.Bool("same-hashes", sameHashes),
				zap.String("revisions", fmt.Sprintf("%+v", revs)),
				zap.String("hashes", fmt.Sprintf("%+v", hashes)),
			)
		}
		time.Sleep(time.Second)
	}

	if err != nil {
		return fmt.Errorf("failed revision and hash check (%v)", err)
	}

	return fmt.Errorf("etcd cluster is not stable: [revisions: %v] and [hashes: %v]", revs, hashes)
}

func (hc *hashChecker) Check() error {
	return hc.checkRevAndHashes()
}

type leaseChecker struct {
	lg  *zap.Logger
	m   *rpcpb.Member
	ls  *leaseStresser
	cli *clientv3.Client
}

func (lc *leaseChecker) Check() error {
	if lc.ls == nil {
		return nil
	}
	if lc.ls != nil &&
		(lc.ls.revokedLeases == nil ||
			lc.ls.aliveLeases == nil ||
			lc.ls.shortLivedLeases == nil) {
		return nil
	}

	cli, err := lc.m.CreateEtcdClient(grpc.WithBackoffMaxDelay(time.Second))
	if err != nil {
		return fmt.Errorf("%v (%q)", err, lc.m.EtcdClientEndpoint)
	}
	defer func() {
		if cli != nil {
			cli.Close()
		}
	}()
	lc.cli = cli

	if err := lc.check(true, lc.ls.revokedLeases.leases); err != nil {
		return err
	}
	if err := lc.check(false, lc.ls.aliveLeases.leases); err != nil {
		return err
	}
	return lc.checkShortLivedLeases()
}

// checkShortLivedLeases ensures leases expire.
func (lc *leaseChecker) checkShortLivedLeases() error {
	ctx, cancel := context.WithTimeout(context.Background(), leaseCheckerTimeout)
	errc := make(chan error)
	defer cancel()
	for leaseID := range lc.ls.shortLivedLeases.leases {
		go func(id int64) {
			errc <- lc.checkShortLivedLease(ctx, id)
		}(leaseID)
	}

	var errs []error
	for range lc.ls.shortLivedLeases.leases {
		if err := <-errc; err != nil {
			errs = append(errs, err)
		}
	}
	return errsToError(errs)
}

func (lc *leaseChecker) checkShortLivedLease(ctx context.Context, leaseID int64) (err error) {
	// retry in case of transient failure or lease is expired but not yet revoked due to the fact that etcd cluster didn't have enought time to delete it.
	var resp *clientv3.LeaseTimeToLiveResponse
	for i := 0; i < retries; i++ {
		resp, err = lc.getLeaseByID(ctx, leaseID)
		// lease not found, for ~v3.1 compatibilities, check ErrLeaseNotFound
		if (err == nil && resp.TTL == -1) || (err != nil && rpctypes.Error(err) == rpctypes.ErrLeaseNotFound) {
			return nil
		}
		if err != nil {
			lc.lg.Debug(
				"retrying; Lease TimeToLive failed",
				zap.Int("retries", i),
				zap.String("lease-id", fmt.Sprintf("%016x", leaseID)),
				zap.Error(err),
			)
			continue
		}
		if resp.TTL > 0 {
			dur := time.Duration(resp.TTL) * time.Second
			lc.lg.Debug(
				"lease has not been expired, wait until expire",
				zap.String("lease-id", fmt.Sprintf("%016x", leaseID)),
				zap.Int64("ttl", resp.TTL),
				zap.Duration("wait-duration", dur),
			)
			time.Sleep(dur)
		} else {
			lc.lg.Debug(
				"lease expired but not yet revoked",
				zap.Int("retries", i),
				zap.String("lease-id", fmt.Sprintf("%016x", leaseID)),
				zap.Int64("ttl", resp.TTL),
				zap.Duration("wait-duration", time.Second),
			)
			time.Sleep(time.Second)
		}
		if err = lc.checkLease(ctx, false, leaseID); err != nil {
			continue
		}
		return nil
	}
	return err
}

func (lc *leaseChecker) checkLease(ctx context.Context, expired bool, leaseID int64) error {
	keysExpired, err := lc.hasKeysAttachedToLeaseExpired(ctx, leaseID)
	if err != nil {
		lc.lg.Warn(
			"hasKeysAttachedToLeaseExpired failed",
			zap.String("endpoint", lc.m.EtcdClientEndpoint),
			zap.Error(err),
		)
		return err
	}
	leaseExpired, err := lc.hasLeaseExpired(ctx, leaseID)
	if err != nil {
		lc.lg.Warn(
			"hasLeaseExpired failed",
			zap.String("endpoint", lc.m.EtcdClientEndpoint),
			zap.Error(err),
		)
		return err
	}
	if leaseExpired != keysExpired {
		return fmt.Errorf("lease %v expiration mismatch (lease expired=%v, keys expired=%v)", leaseID, leaseExpired, keysExpired)
	}
	if leaseExpired != expired {
		return fmt.Errorf("lease %v expected expired=%v, got %v", leaseID, expired, leaseExpired)
	}
	return nil
}

func (lc *leaseChecker) check(expired bool, leases map[int64]time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), leaseCheckerTimeout)
	defer cancel()
	for leaseID := range leases {
		if err := lc.checkLease(ctx, expired, leaseID); err != nil {
			return err
		}
	}
	return nil
}

// TODO: handle failures from "grpc.FailFast(false)"
func (lc *leaseChecker) getLeaseByID(ctx context.Context, leaseID int64) (*clientv3.LeaseTimeToLiveResponse, error) {
	return lc.cli.TimeToLive(
		ctx,
		clientv3.LeaseID(leaseID),
		clientv3.WithAttachedKeys(),
	)
}

func (lc *leaseChecker) hasLeaseExpired(ctx context.Context, leaseID int64) (bool, error) {
	// keep retrying until lease's state is known or ctx is being canceled
	for ctx.Err() == nil {
		resp, err := lc.getLeaseByID(ctx, leaseID)
		if err != nil {
			// for ~v3.1 compatibilities
			if rpctypes.Error(err) == rpctypes.ErrLeaseNotFound {
				return true, nil
			}
		} else {
			return resp.TTL == -1, nil
		}
		lc.lg.Warn(
			"hasLeaseExpired getLeaseByID failed",
			zap.String("endpoint", lc.m.EtcdClientEndpoint),
			zap.String("lease-id", fmt.Sprintf("%016x", leaseID)),
			zap.Error(err),
		)
	}
	return false, ctx.Err()
}

// The keys attached to the lease has the format of "<leaseID>_<idx>" where idx is the ordering key creation
// Since the format of keys contains about leaseID, finding keys base on "<leaseID>" prefix
// determines whether the attached keys for a given leaseID has been deleted or not
func (lc *leaseChecker) hasKeysAttachedToLeaseExpired(ctx context.Context, leaseID int64) (bool, error) {
	resp, err := lc.cli.Get(ctx, fmt.Sprintf("%d", leaseID), clientv3.WithPrefix())
	if err != nil {
		lc.lg.Warn(
			"hasKeysAttachedToLeaseExpired failed",
			zap.String("endpoint", lc.m.EtcdClientEndpoint),
			zap.String("lease-id", fmt.Sprintf("%016x", leaseID)),
			zap.Error(err),
		)
		return false, err
	}
	return len(resp.Kvs) == 0, nil
}

// compositeChecker implements a checker that runs a slice of Checkers concurrently.
type compositeChecker struct{ checkers []Checker }

func newCompositeChecker(checkers []Checker) Checker {
	return &compositeChecker{checkers}
}

func (cchecker *compositeChecker) Check() error {
	errc := make(chan error)
	for _, c := range cchecker.checkers {
		go func(chk Checker) { errc <- chk.Check() }(c)
	}
	var errs []error
	for range cchecker.checkers {
		if err := <-errc; err != nil {
			errs = append(errs, err)
		}
	}
	return errsToError(errs)
}

type runnerChecker struct {
	errc chan error
}

func (rc *runnerChecker) Check() error {
	select {
	case err := <-rc.errc:
		return err
	default:
		return nil
	}
}

type noChecker struct{}

func newNoChecker() Checker        { return &noChecker{} }
func (nc *noChecker) Check() error { return nil }
