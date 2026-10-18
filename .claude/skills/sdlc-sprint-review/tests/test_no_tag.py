"""Review reports can close a sprint without a release tag."""
import unittest
from test_report import example, decided, report


def no_tag(language='en'):
    data=example(language)
    data['release'].update(publishTag=False,tag=None)
    data['decision']['tagPublication']['status']='not_required'
    return data


class NoTagTests(unittest.TestCase):
    def test_report_renders_in_both_languages(self):
        for language, label in [('en','No tag planned'),('fr','Aucun tag prévu')]:
            data=no_tag(language); report.validate(data)
            html,_=report.render(data)
            self.assertIn(label, html)
            self.assertNotIn('>None<',html)

    def test_closed_no_tag_report_still_needs_exact_human_approval(self):
        data=no_tag(); data['sprint']['state']='closed'
        with self.assertRaises(ValueError): report.validate(data)
        decided(data); report.validate(data)
        for field, wrong in [('candidate','e'*40),('reportDigest',None),('scopeDigest','e'*64),('tag','v9.0.0')]:
            bad=no_tag(); decided(bad); bad['sprint']['state']='closed'
            bad['decision']['approval'][field]=wrong
            with self.subTest(field=field), self.assertRaises(ValueError): report.validate(bad)

    def test_tag_choice_and_publication_cannot_contradict(self):
        for mutate in [lambda d:d['release'].update(tag='v9.0.0'),lambda d:d['release'].update(publishTag='false'),lambda d:d['release'].update(publishTag=0),lambda d:d['decision']['tagPublication'].update(status='published'),lambda d:d['decision']['tagPublication'].update(object='a'*40),lambda d:d['decision']['tagPublication'].update(status='not_published')]:
            data=no_tag();mutate(data)
            with self.assertRaises(ValueError): report.validate(data)


class PacketIdentityTests(unittest.TestCase):
    def test_packet_identity_is_validated_and_visible_in_hashed_source(self):
        for language in ['en','fr']:
            data=example(language)
            report.validate(data)
            html,source=report.render(data)
            self.assertIn('<code>'+data['report']['packetDigest']+'</code>',html)
            data['report']['packetDigest']='d'*64
            changed,digest=report.render(data)
            self.assertNotEqual(source,digest)
            self.assertIn('<code>'+'d'*64+'</code>',changed)
            self.assertNotEqual(html,changed)
        for value in [None, 4, 'bad', 'e'*63]:
            data=example();data['report']['packetDigest']=value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError,'invalid digest'):
                report.validate_identity(data)

    def test_legacy_report_can_render_without_claiming_packet_evidence(self):
        data=example();data['report'].pop('packetDigest')
        report.validate(data)
        html,_=report.render(data)
        self.assertIn('Review packet SHA-256',html)
