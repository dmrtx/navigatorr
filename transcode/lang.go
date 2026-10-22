package transcode

import (
	"sort"
	"strings"
)

// LanguageUndetermined is the canonical code of an untagged or "und" track.
const LanguageUndetermined = "und"

// iso6392 is every ISO 639-2 alpha-3 code in its bibliographic form (the
// terminology form of the 20 languages that have two is in languageTerminology).
// Reserved local codes qaa-qtz are not listed: nothing can mean them.
var iso6392 = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`aar abk ace ach ada ady afa afh afr ain aka akk alb ale alg alt amh ang anp apa ara arc arg arm arn arp art arw asm ast ath aus ava ave awa aym aze
bad bai bak bal bam ban baq bas bat bej bel bem ben ber bho bih bik bin bis bla bnt bos bra bre btk bua bug bul bur byn
cad cai car cat cau ceb cel cha chb che chg chi chk chm chn cho chp chr chu chv chy cmc cnr cop cor cos cpe cpf cpp cre crh crp csb cus cze
dak dan dar day del den dgr din div doi dra dsb dua dum dut dyu dzo efi egy eka elx eng enm epo est ewe ewo fan fao fat fij fil fin fiu fon fre frm fro frr frs fry ful fur
gaa gay gba gem geo ger gez gil gla gle glg glv gmh goh gon gor got grb grc gre grn gsw guj gwi hai hat hau haw heb her hil him hin hit hmn hmo hrv hsb hun hup
iba ibo ice ido iii ijo iku ile ilo ina inc ind ine inh ipk ira iro ita jav jbo jpn jpr jrb kaa kab kac kal kam kan kar kas kau kaw kaz kbd kha khi khm kho kik kin kir kmb kok kom kon kor kos kpe krc krl kro kru kua kum kur kut
lad lah lam lao lat lav lez lim lin lit lol loz ltz lua lub lug lui lun luo lus mac mad mag mah mai mak mal man mao map mar mas may mdf mdr men mga mic min mis mkh mlg mlt mnc mni mno moh mon mos mul mun mus mwl mwr myn myv
nah nai nap nau nav nbl nde ndo nds nep new nia nic niu nno nob nog non nor nqo nso nub nwc nya nym nyn nyo nzi oci oji ori orm osa oss ota oto paa pag pal pam pan pap pau peo per phi phn pli pol pon por pra pro pus que
raj rap rar roa roh rom rum run rup rus sad sag sah sai sal sam san sas sat scn sco sel sem sga sgn shn sid sin sio sit sla slo slv sma sme smi smj smn smo sms sna snd snk sog som son sot spa srd srn srp srr ssa ssw suk sun sus sux swa swe syc syr
tah tai tam tat tel tem ter tet tgk tgl tha tib tig tir tiv tkl tlh tli tmh tog ton tpi tsi tsn tso tuk tum tup tur tut tvl twi tyv udm uga uig ukr umb und urd uzb vai ven vie vol vot wak wal war was wel wen wln wol xal xho yao yap yid yor ypk zap zbl zen zgh zha znd zul zun zxx zza`) {
		m[c] = true
	}
	return m
}()

// languageTerminology maps the bibliographic code of the 20 languages that
// have two ISO 639-2 codes to the terminology code, which is the canonical
// form: ger and deu are the same language, as are fre/fra, chi/zho and so on.
var languageTerminology = map[string]string{
	"alb": "sqi", "arm": "hye", "baq": "eus", "bur": "mya", "chi": "zho",
	"cze": "ces", "dut": "nld", "fre": "fra", "geo": "kat", "ger": "deu",
	"gre": "ell", "ice": "isl", "mac": "mkd", "mao": "mri", "may": "msa",
	"per": "fas", "rum": "ron", "slo": "slk", "tib": "bod", "wel": "cym",
}

// languageTwoLetter maps the ISO 639-1 codes tracks are tagged with (MP4 and
// BCP 47 style tags such as es-419) to the canonical three-letter code.
var languageTwoLetter = map[string]string{
	"ar": "ara", "bg": "bul", "ca": "cat", "cs": "ces", "cy": "cym", "da": "dan",
	"de": "deu", "el": "ell", "en": "eng", "es": "spa", "et": "est", "eu": "eus",
	"fa": "fas", "fi": "fin", "fr": "fra", "ga": "gle", "gl": "glg", "he": "heb",
	"hi": "hin", "hr": "hrv", "hu": "hun", "hy": "hye", "id": "ind", "is": "isl",
	"it": "ita", "ja": "jpn", "ka": "kat", "ko": "kor", "la": "lat", "lt": "lit",
	"lv": "lav", "mk": "mkd", "ms": "msa", "nb": "nob", "nl": "nld", "nn": "nno",
	"no": "nor", "pl": "pol", "pt": "por", "ro": "ron", "ru": "rus", "sk": "slk",
	"sl": "slv", "sq": "sqi", "sr": "srp", "sv": "swe", "ta": "tam", "te": "tel",
	"th": "tha", "tl": "tgl", "tr": "tur", "uk": "ukr", "vi": "vie", "zh": "zho",
}

// ValidLanguageCode reports whether code is an ISO 639-2 alpha-3 code, in
// either its bibliographic or its terminology form, in lower case.
func ValidLanguageCode(code string) bool {
	if iso6392[code] {
		return true
	}
	for _, t := range languageTerminology {
		if t == code {
			return true
		}
	}
	return false
}

// CanonicalLanguage is the single form two spellings of one language share.
// Case, locale subtags (es-419, pt_BR) and the ISO 639-1 spelling are folded,
// and the bibliographic code of a language with two maps to its terminology
// code (ger and deu are both "deu"). A tag that is empty, "und" or "unknown"
// is "und"; a tag nothing recognizes is returned lower-cased as written, so it
// matches only itself.
func CanonicalLanguage(tag string) string {
	l := strings.ToLower(strings.TrimSpace(tag))
	if l == "" || l == "unknown" {
		return LanguageUndetermined
	}
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	if t, ok := languageTwoLetter[l]; ok {
		return t
	}
	if t, ok := languageTerminology[l]; ok {
		return t
	}
	return l
}

// InvalidLanguageError names a code that is not an ISO 639-2 language code.
type InvalidLanguageError struct{ Code string }

func (e *InvalidLanguageError) Error() string {
	return "\"" + e.Code + "\" is not an ISO 639-2 language code (three letters, such as eng, spa or jpn)"
}

// LanguageList validates a list of languages to keep and returns it in
// canonical form: lower-cased ISO 639-2 codes folded to one spelling per
// language, without duplicates, sorted. A code outside ISO 639-2 is an error
// naming it. The list is never nil.
func LanguageList(codes []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range codes {
		code := strings.ToLower(strings.TrimSpace(raw))
		if !ValidLanguageCode(code) {
			return nil, &InvalidLanguageError{Code: raw}
		}
		c := CanonicalLanguage(code)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DefaultKeepLanguages is the global default of languages to keep: Spanish,
// English and Japanese.
func DefaultKeepLanguages() []string { return []string{"eng", "jpn", "spa"} }
