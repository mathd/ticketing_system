"""A full review is required even when the owner does not publish a tag."""
import copy
import unittest
from test_sprint_state import close_snapshot, decide, progress, start_snapshot


def no_tag():
    data = close_snapshot()
    data['brief']['release'] = False
    data['release'].pop('publication')
    data['release'].pop('tagObject')
    data['release']['tag'] = None
    data['closeApproval']['tag'] = None
    return data


class ReviewClosureTests(unittest.TestCase):
    def test_review_and_report_without_tag_can_close(self):
        self.assertEqual(decide(no_tag(), 'close'), ('CLOSE', []))

    def test_done_and_owner_approval_do_not_replace_review_and_report(self):
        mutations = [
            lambda d: d.pop('release'),
            lambda d: d['release'].pop('review'),
            lambda d: d['release']['review'].update(decision='FIX'),
            lambda d: d['release'].pop('report'),
            lambda d: d['release']['report'].update(verified=False),
            lambda d: d['release']['report'].update(pointerVerified=False),
            lambda d: d['release']['report'].update(candidate='e'*40),
            lambda d: d['release']['report'].update(packetDigest='e'*64),
            lambda d: d['release']['report'].update(htmlDigest='e'*64),
            lambda d: d['release']['report'].update(commit=None),
            lambda d: d['release']['report'].update(path=None),
            lambda d: d['release']['report'].update(sourceDigest=None),
            lambda d: d['closeApproval'].update(reportDigest='e'*64),
            lambda d: d['closeApproval'].update(candidate='e'*40),
            lambda d: d['closeApproval'].update(tag='v9.0.0'),
        ]
        for i, mutate in enumerate(mutations):
            with self.subTest(i=i):
                data = no_tag(); mutate(data)
                self.assertEqual(decide(data, 'close')[0], 'STOP')

    def test_each_area_must_complete_on_the_same_packet_and_round(self):
        for area in no_tag()['release']['review']['areas']:
            for field, value in [('decision','FIX'),('round',2),('packetDigest','e'*64),('recordDigest',None)]:
                with self.subTest(area=area, field=field):
                    data = no_tag(); data['release']['review']['areas'][area][field] = value
                    self.assertEqual(decide(data, 'close')[0], 'STOP')
            data = no_tag(); del data['release']['review']['areas'][area]
            self.assertEqual(decide(data, 'close')[0], 'STOP')

    def test_old_brief_remains_readable_but_needs_explicit_policy_before_action(self):
        for intent, factory in [('start',start_snapshot),('close',close_snapshot)]:
            data = factory(); del data['brief']['review']
            progress(data)
            self.assertEqual(decide(data, intent)[0], 'STOP')

    def test_skipping_review_requires_a_reason_and_cannot_publish(self):
        data = no_tag(); data['brief'].update(review='none', closureTask=None)
        for field in ['candidate', 'reportDigest', 'tag']:
            data['closeApproval'].pop(field, None)
        self.assertEqual(decide(data, 'close')[0], 'STOP')
        data['brief']['reviewReason']='Owner approved a planning exercise.'
        self.assertEqual(decide(data, 'close')[0], 'CLOSE')
        data['brief']['release']=True
        self.assertEqual(decide(data, 'close')[0], 'STOP')
