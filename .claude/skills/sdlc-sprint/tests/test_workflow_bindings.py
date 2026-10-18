"""Bind owner priorities and reviewed commits to lifecycle operations."""
import copy
import unittest
from test_sprint_state import close_snapshot, start_snapshot, decide, progress
from test_review_closure import no_tag


class WorkflowBindingTests(unittest.TestCase):
    def test_current_report_does_not_substitute_another_reviewed_commit(self):
        for factory in [close_snapshot,no_tag]:
            data=factory()
            data['release']['candidate']='e'*40
            data['release']['report']['candidate']='e'*40
            data['closeApproval']['candidate']='e'*40
            self.assertEqual(decide(data,'close')[0],'STOP')

    @staticmethod
    def corrected():
        data=no_tag(); old=data['release']['candidate']; new='e'*40
        data['release']['candidate']=new
        data['release']['report']['candidate']=new
        data['closeApproval']['candidate']=new
        data['release']['review']['correction']={
            'fromCandidate':old,'toCandidate':new,'diffDigest':'a'*64,'recordDigest':'b'*64,
            'verificationPassed':True,'materialChange':False,'repeatThresholdFinding':False,
            'gatePassed':True,'gateCandidate':new}
        return data

    def test_verified_bounded_correction_needs_no_extra_formal_round(self):
        self.assertEqual(decide(self.corrected(),'close'),('CLOSE',[]))
        for field in self.corrected()['release']['review']['correction']:
            with self.subTest(field=field):
                data=self.corrected(); del data['release']['review']['correction'][field]
                self.assertEqual(decide(data,'close')[0],'STOP')
        for field,value in [('materialChange',True),('repeatThresholdFinding',True),('gateCandidate','f'*40),('fromCandidate','f'*40),('toCandidate','f'*40)]:
            data=self.corrected();data['release']['review']['correction'][field]=value
            self.assertEqual(decide(data,'close')[0],'STOP')

    def test_review_identity_and_no_review_closure_are_explicit(self):
        for value in [None, 'invalid', 'e'*39]:
            data=no_tag();data['release']['review']['candidate']=value
            self.assertEqual(decide(data,'close')[0],'STOP')
        data=no_tag();data['brief'].update(review='none',reviewReason='Owner planning choice.')
        for field in ['candidate','reportDigest','tag']:data['closeApproval'].pop(field,None)
        self.assertEqual(decide(data,'close')[0],'STOP')
        for value in [False, 1, [], 'skip']:
            data=no_tag();data['brief']['review']=value
            with self.assertRaises((ValueError,TypeError)):progress(data)

    def test_tag_plan_also_needs_report_packet_identity(self):
        data=close_snapshot();del data['release']['report']['packetDigest']
        self.assertEqual(decide(data,'close')[0],'STOP')

    def test_forbidden_targets_do_not_hide_in_no_tag_or_no_review_records(self):
        data=no_tag();data['release']['tagObject']='a'*40
        self.assertEqual(decide(data,'close')[0],'STOP')
        data=no_tag();data['brief'].update(review='none',reviewReason='Owner chose planning only.',closureTask=None)
        data.pop('release')
        for field in ['candidate','reportDigest','tag']: data['closeApproval'].pop(field,None)
        self.assertEqual(decide(data,'close')[0],'CLOSE')
        for field in ['candidate','reportDigest','tag']:
            bad=copy.deepcopy(data);bad['closeApproval'][field]='a'*64
            self.assertEqual(decide(bad,'close')[0],'STOP')
        data['brief']['closureTask']='DEMO-3'
        self.assertEqual(decide(data,'close')[0],'STOP')

    def test_continuation_is_explicit_before_start_and_close(self):
        for intent,factory in [('start',start_snapshot),('close',close_snapshot)]:
            for mode in ['autonomous','manual']:
                data=factory();data['brief']['continuation']=mode
                self.assertEqual(decide(data,intent)[0],intent.upper())
            data=factory();data['brief'].pop('continuation',None)
            progress(data)
            self.assertEqual(decide(data,intent)[0],'STOP')
            for value in ['sometimes',False,1,[]]:
                bad=factory();bad['brief']['continuation']=value
                with self.assertRaises((ValueError,TypeError)):progress(bad)

    def test_automatic_start_binds_later_ready_moves_to_exact_owner_scope(self):
        data=start_snapshot();self.assertEqual(decide(data)[0],'START')
        for field,value in [('sprint','S-99'),('briefDigest','e'*64),('scopeDigest','e'*64),('members',['DEMO-1']),('approvedBy',None),('approvedAt','2026-12-01T00:00:00Z'),('digest',None)]:
            with self.subTest(field=field):
                bad=copy.deepcopy(data);bad['priorityApproval'][field]=value
                self.assertEqual(decide(bad)[0],'STOP')
        data.pop('priorityApproval')
        self.assertEqual(decide(data)[0],'STOP')
        # Manual continuation can wait for Gate 1 later; it must not claim permission.
        data['brief']['continuation']='manual'
        self.assertEqual(decide(data)[0],'START')
