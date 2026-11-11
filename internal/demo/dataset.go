package demo

import (
	"fmt"
	"time"

	"github.com/accquix/register/internal/synthetic"
)

// Set is the fictional data set: one RPO snapshot and the six Financial Administration lists.
type Set struct {
	SnapshotDate string // YYYY-MM-DD
	ListDate     string // YYYY-MM-DD
	Records      []synthetic.Rec
	Lists        map[string][]synthetic.Item
}

// Every name, place and identifier below is invented. The IČOs are 999xxxxx with a valid check
// digit, a range the register does not use; DIČs and IČ DPHs are derived from them.
var (
	adjectives = []string{"Zlatá", "Strieborná", "Modrá", "Zelená", "Tichá", "Veselá", "Horská", "Severná", "Slnečná", "Lesná", "Jarná", "Biela"}
	nouns      = []string{"Rybka", "Lipa", "Hviezda", "Brána", "Kotva", "Lúka", "Včela", "Sova", "Breza", "Studnička"}
	trades     = []string{"stavby", "logistika", "pekáreň", "software", "doprava", "tlačiareň", "účtovníctvo", "záhrady"}
	towns      = []struct{ name, psc string }{
		{"Fiktívne Mesto", "999 01"}, {"Vymyslená Ves", "999 02"}, {"Snová Dolina", "999 03"}, {"Ukážkovo", "999 04"}, {"Príkladov", "999 05"},
	}
	streets  = []string{"Hlavná", "Školská", "Záhradná", "Lipová", "Mlynská", "Nová", "Krátka", "Poľná"}
	men      = []string{"Ján", "Peter", "Martin", "Tomáš", "Michal"}
	women    = []string{"Mária", "Eva", "Zuzana", "Katarína", "Lucia"}
	surnames = []struct{ m, f string }{
		{"Fiktívny", "Fiktívna"}, {"Vymyslený", "Vymyslená"}, {"Ukážkový", "Ukážková"}, {"Skúšobný", "Skúšobná"}, {"Príkladný", "Príkladná"}, {"Nápadný", "Nápadná"},
	}
	crafts = []string{"Kaderníctvo", "Účtovníctvo", "Elektroinštalácie", "Maliarstvo", "Fotografia", "Krajčírstvo"}
)

// ico returns 999 + a 4-digit number + the IČO check digit (weights 8..2, mod 11).
func ico(n int) string {
	base := fmt.Sprintf("999%04d", n)
	sum := 0
	for i, c := range base {
		sum += int(c-'0') * (8 - i)
	}
	return fmt.Sprintf("%s%d", base, (11-sum%11)%10)
}

func skDate(t time.Time) string { return t.Format("02.01.2006") }

// Dataset builds the fictional data set as of now: 120 companies and 60 sole traders, with
// VAT payers and §7/§7a registrations, one VAT group, closed subjects, renamed ones, reliability
// indexes and a few entries on each risk list.
func Dataset(now time.Time) Set {
	day := func(d int) time.Time { return now.AddDate(0, 0, d) }
	set := Set{SnapshotDate: day(-1).Format("2006-01-02"), ListDate: day(0).Format("2006-01-02"), Lists: map[string][]synthetic.Item{}}
	add := func(slug string, it synthetic.Item) { set.Lists[slug] = append(set.Lists[slug], it) }
	for _, slug := range []string{"ds_dphs", "ds_dsrdp", "ds_iz_ran", "ds_dphz", "ds_dphv", "ds_dpho"} {
		set.Lists[slug] = []synthetic.Item{}
	}
	place := func(n int) (string, string, string, int64) {
		t := towns[n%len(towns)]
		return streets[n%len(streets)], t.name, t.psc, int64(100 + n)
	}
	n := 0

	// companies: every adjective with every noun, some with a trade, as s.r.o. or a.s.
	for i, adj := range adjectives {
		for j, noun := range nouns {
			n++
			name := adj + " " + noun
			if (i*len(nouns)+j)%4 == 1 {
				name += " " + trades[(i+j)%len(trades)]
			}
			rec := synthetic.Company(n, name)
			rec.ICO, rec.ID = ico(n), 900_000_000+int64(n)
			rec.Street, rec.Municipality, rec.PSC, rec.RegNumber = place(n)
			rec.Building = fmt.Sprint(1 + n%40)
			rec.Established = day(-365 * (1 + n%15)).Format("2006-01-02")
			rec.RegFrom = rec.Established
			rec.Office, rec.Number = "Okresný súd Fiktívny", fmt.Sprintf("Sro/%d/F", 1000+n)
			if n%5 == 0 {
				rec.Name += ", a.s."
				rec.LegalCode, rec.LegalForm, rec.Number = "121", "Akciová spoločnosť", fmt.Sprintf("Sa/%d/F", 1000+n)
			} else {
				rec.Name += ", s.r.o."
			}
			if n%20 == 7 {
				rec.OldNames = []string{adj + " " + noun + " Pôvodná, s.r.o."}
			}
			if n%15 == 0 {
				rec.Terminated = day(-200).Format("2006-01-02")
			}
			set.Records = append(set.Records, rec)
			companyLists(add, n, rec, day)
		}
	}

	// sole traders: fictional people with a craft
	for k := 0; k < 60; k++ {
		n++
		s := surnames[k%len(surnames)]
		person := men[k%len(men)] + " " + s.m
		if k%2 == 1 {
			person = women[k%len(women)] + " " + s.f
		}
		rec := synthetic.Trader(n, ico(n), person+" - "+crafts[k%len(crafts)])
		rec.ID = 900_000_000 + int64(n)
		rec.Street, rec.Municipality, rec.PSC, rec.RegNumber = place(n)
		rec.Building = fmt.Sprint(1 + n%30)
		rec.Established = day(-90 * (1 + k%20)).Format("2006-01-02")
		rec.RegFrom, rec.Number = rec.Established, fmt.Sprintf("999-%d", 5000+n)
		if k%12 == 11 {
			rec.Terminated = day(-60).Format("2006-01-02")
		}
		set.Records = append(set.Records, rec)
		dic := "10" + rec.ICO
		if k%10 == 9 { // registered days ago: an IČO, no DIČ yet (D38)
			continue
		}
		add("ds_dsrdp", taxItem(dic, rec))
		switch k % 4 {
		case 0:
			add("ds_dphs", vatItem("SK"+dic, rec, "§4", day(-300), day(-300)))
		case 1:
			add("ds_dphs", vatItem("SK"+dic, rec, "§7a", day(-120), time.Time{})) // registered, not a VAT payer
		}
		if k%3 == 0 {
			add("ds_iz_ran", synthetic.Item{"DIC": dic, "ICO": rec.ICO, "NAZOV_SUBJEKTU": rec.Name, "IDS": "spoľahlivý"})
		}
	}
	return set
}

func companyLists(add func(string, synthetic.Item), n int, rec synthetic.Rec, day func(int) time.Time) {
	dic := "20" + rec.ICO
	icdph := "SK" + dic
	add("ds_dsrdp", taxItem(dic, rec))
	switch {
	case n == 3 || n == 4: // one VAT group: two members share the IČ DPH of the first
		it := vatItem("SK20"+ico(3), rec, "§4b", day(-400), day(-400))
		add("ds_dphs", it)
	case n%6 == 1:
		add("ds_dphs", vatItem(icdph, rec, "§7", day(-500), time.Time{})) // registered for EU services, not a payer
	case n%6 != 5:
		add("ds_dphs", vatItem(icdph, rec, "§4", day(-700), day(-700)))
	}
	switch n % 4 {
	case 0:
		add("ds_iz_ran", synthetic.Item{"DIC": dic, "ICO": rec.ICO, "NAZOV_SUBJEKTU": rec.Name, "IDS": "spoľahlivý"})
	case 1:
		add("ds_iz_ran", synthetic.Item{"DIC": dic, "ICO": rec.ICO, "NAZOV_SUBJEKTU": rec.Name, "IDS": "menej spoľahlivý"})
	}
	switch n {
	case 11:
		add("ds_iz_ran", synthetic.Item{"DIC": dic, "ICO": rec.ICO, "NAZOV_SUBJEKTU": rec.Name, "IDS": "nespoľahlivý"})
		add("ds_dphz", synthetic.Item{"IC_DPH": icdph, "ICO": rec.ICO, "NAZOV": rec.Name, "ROK_PORUSENIA": fmt.Sprint(day(-400).Year()), "DAT_ZVEREJNENIA": skDate(day(-300))})
	case 12:
		add("ds_dphv", synthetic.Item{"IC_DPH": icdph, "ICO": rec.ICO, "NAZOV": rec.Name, "DAT_ZVEREJNENIA": skDate(day(-90)), "DAT_VYMAZU": ""})
	case 13:
		add("ds_dpho", synthetic.Item{"IC_DPH": icdph, "ICO": rec.ICO, "NAZOV": rec.Name, "DAT_ZACATIA": skDate(day(-150))})
	}
}

func address(rec synthetic.Rec) string { return rec.Street + " " + rec.Building }

func taxItem(dic string, rec synthetic.Rec) synthetic.Item {
	return synthetic.Item{"DIC": dic, "ICO": rec.ICO, "NAZOV_DS": rec.Name, "OBEC": rec.Municipality, "PSC": rec.PSC,
		"ULICA_CISLO": address(rec), "NAZOV_STATU": "Slovensko"}
}

func vatItem(icdph string, rec synthetic.Rec, paragraph string, registered, payerFrom time.Time) synthetic.Item {
	it := synthetic.Item{"IC_DPH": icdph, "ICO": rec.ICO, "NAZOV_DS": rec.Name, "OBEC": rec.Municipality, "PSC": rec.PSC,
		"ULICA_CISLO": address(rec), "STAT": "Slovensko", "DRUH_REG_DPH": paragraph, "DATUM_REG": skDate(registered)}
	if !payerFrom.IsZero() {
		it["PLAT_DPH_OD"] = skDate(payerFrom)
	}
	return it
}
